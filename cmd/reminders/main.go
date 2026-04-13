package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/pmollerus23/reminders/internal/chat"
	"github.com/pmollerus23/reminders/internal/config"
	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/fact"
	"github.com/pmollerus23/reminders/internal/handler"
	"github.com/pmollerus23/reminders/internal/httpserver"
	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/memory/summarize"
	"github.com/pmollerus23/reminders/internal/promptctx"
	"github.com/pmollerus23/reminders/internal/reminder"
	"github.com/pmollerus23/reminders/internal/scheduler"
	"github.com/pmollerus23/reminders/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger.Info("starting reminders", "env", cfg.Env)

	// --- Database ---
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()
	logger.Info("database connected")

	if err := db.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	logger.Info("migrations applied")

	// --- Memory store ---
	memStore := memory.NewPostgresStore(pool)
	logger.Info("memory store constructed")

	// --- Prompt context builder ---
	const baseSystemPrompt = "You are a helpful reminder assistant. Help the user set and manage reminders."
	chatBuilder := promptctx.New(memStore, baseSystemPrompt, cfg.VerbatimTurns)

	// --- Telegram client ---
	tg, err := telegram.New(ctx, cfg.TelegramBotToken, logger)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	logger.Info("telegram client constructed")

	// --- Reminder parser ---
	var reminderParser reminder.Parser
	switch cfg.Parser {
	case config.ParserRegex:
		reminderParser = reminder.NewRegexParser()
	case config.ParserClaude:
		reminderParser = reminder.NewClaudeParser(cfg.AnthropicAPIKey, logger)
	default:
		return fmt.Errorf("unknown PARSER: %q", cfg.Parser)
	}

	// --- Fact parser and chat responder ---
	// Both /remember and /chat only make sense with LLM parsing; the stubs make
	// that clear rather than silently succeeding with wrong output. Both share
	// PARSER to avoid redundant env vars.
	var factParser fact.Parser
	var chatResponder chat.Responder
	switch cfg.Parser {
	case config.ParserRegex:
		factParser = fact.NewStubParser()
		chatResponder = chat.NewStubResponder()
	case config.ParserClaude:
		factParser = fact.NewClaudeParser(cfg.AnthropicAPIKey, logger)
		chatResponder = chat.NewClaudeResponder(cfg.AnthropicAPIKey, logger)
	}

	// --- Inbound handler ---
	h := handler.New(pool, tg, reminderParser, factParser, memStore, chatBuilder, chatResponder, cfg.Location, logger)
	tg.AttachHandler(h)

	// --- HTTP server ---
	server := httpserver.New(logger, cfg.HTTPAddr)

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("http server listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http listen: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		return scheduler.Run(gCtx, pool, tg, logger)
	})

	g.Go(func() error {
		return tg.Start(gCtx)
	})

	// --- Summarize loop ---
	// Only runs when PARSER=claude — no API key means no summarizer.
	if cfg.Parser == config.ParserClaude {
		summarizer := summarize.NewClaudeSummarizer(cfg.AnthropicAPIKey, logger)
		loop := summarize.NewLoop(memStore, summarizer, cfg.VerbatimTurns, cfg.SummarizeInterval, logger)
		g.Go(func() error {
			return loop.Run(gCtx)
		})
	} else {
		logger.Info("summarize loop disabled (PARSER != claude)")
	}

	g.Go(func() error {
		<-gCtx.Done()
		logger.Info("shutdown signal received")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		logger.Info("http server stopped")
		return nil
	})

	return g.Wait()
}

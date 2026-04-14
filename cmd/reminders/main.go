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

	"github.com/pmollerus23/reminders/internal/config"
	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/handler"
	"github.com/pmollerus23/reminders/internal/httpserver"
	"github.com/pmollerus23/reminders/internal/intent"
	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/memory/summarize"
	"github.com/pmollerus23/reminders/internal/proactive"
	"github.com/pmollerus23/reminders/internal/promptctx"
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
	// The base system prompt describes all three capabilities so the unified
	// dispatcher can steer the model toward the right tool.
	const baseSystemPrompt = "You are a personal assistant. You can set reminders, remember personal facts, and have open-ended conversations."
	chatBuilder := promptctx.New(memStore, baseSystemPrompt, cfg.VerbatimTurns)

	// --- Telegram client ---
	tg, err := telegram.New(ctx, cfg.TelegramBotToken, logger)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	logger.Info("telegram client constructed")

	// --- Unified dispatcher ---
	// The dispatcher handles all three intents in one API call. When PARSER=regex
	// there is no API key, so the stub returns a clear error message instead.
	var dispatcher intent.Dispatcher
	switch cfg.Parser {
	case config.ParserRegex:
		dispatcher = intent.NewStubDispatcher()
		logger.Info("using stub dispatcher (PARSER != claude)")
	case config.ParserClaude:
		dispatcher = intent.NewClaudeDispatcher(cfg.AnthropicAPIKey, logger)
		logger.Info("using Claude dispatcher")
	default:
		return fmt.Errorf("unknown PARSER: %q", cfg.Parser)
	}

	// --- Inbound handler ---
	h := handler.New(pool, tg, dispatcher, memStore, chatBuilder, cfg.Location, logger)
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

	// --- Summarize loop + proactive loop ---
	// Both require the Anthropic API; only run when PARSER=claude.
	if cfg.Parser == config.ParserClaude {
		summarizer := summarize.NewClaudeSummarizer(cfg.AnthropicAPIKey, logger)
		sumLoop := summarize.NewLoop(memStore, summarizer, cfg.VerbatimTurns, cfg.SummarizeInterval, logger)
		g.Go(func() error {
			return sumLoop.Run(gCtx)
		})

		proAgent := proactive.NewClaudeAgent(cfg.AnthropicAPIKey, logger)
		proLoop := proactive.NewLoop(pool, memStore, tg, proAgent, cfg.ProactiveInterval, cfg.Location, logger)
		g.Go(func() error {
			return proLoop.Run(gCtx)
		})
	} else {
		logger.Info("summarize and proactive loops disabled (PARSER != claude)")
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

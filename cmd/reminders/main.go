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
	"github.com/pmollerus23/reminders/internal/httpserver"
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

	// --- Smoke test (temporary; remove when scheduler lands in M4) ---
	smokeCtx, cancelSmoke := context.WithTimeout(ctx, 5*time.Second)
	if _, err := db.GetDueReminders(smokeCtx, pool, 1); err != nil {
		cancelSmoke()
		return fmt.Errorf("db smoke test: %w", err)
	}
	cancelSmoke()
	logger.Info("db smoke test ok")

	// --- HTTP server (existing) ---
	server := httpserver.New(logger, ":8080")

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("http server listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http listen: %w", err)
		}
		return nil
	})

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

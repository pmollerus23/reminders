package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Run(ctx context.Context, pool *pgxpool.Pool, msgr Messenger, logger *slog.Logger) error {
	const tickInterval = 5 * time.Second
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	logger.Info("scheduler started", "interval", tickInterval)

	for {
		select {
		case <-ctx.Done():
			logger.Info("scheduler shutting down")
			return nil
		case <-ticker.C:
			if err := claimAndProcess(ctx, pool, msgr, logger); err != nil {
				logger.Error("scheduler tick failed", "err", err)
			}
		}
	}
}

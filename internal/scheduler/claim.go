package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pmollerus23/reminders/internal/db"
)

const (
	batchSize     = 100
	leaseDuration = 60 * time.Second
)

const claimQuery = `
UPDATE reminders
SET status = 'processing',
    locked_until = now() + ($1 * interval '1 second')
WHERE id IN (
    SELECT id FROM reminders
    WHERE scheduled_at <= now()
      AND (
        status = 'pending'
        OR (status = 'processing' AND locked_until < now())
      )
    ORDER BY scheduled_at
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
RETURNING id, telegram_chat_id, body, scheduled_at;
`

func claimAndProcess(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, claimQuery, int64(leaseDuration.Seconds()), batchSize)
	if err != nil {
		return fmt.Errorf("claim query: %w", err)
	}

	var claimed []db.Reminder
	for rows.Next() {
		var r db.Reminder
		if err := rows.Scan(&r.ID, &r.TelegramChatID, &r.Body, &r.ScheduledAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan claimed row: %w", err)
		}
		claimed = append(claimed, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate claimed rows: %w", err)
	}
	rows.Close()

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit claim tx: %w", err)
	}

	if len(claimed) == 0 {
		return nil
	}

	logger.Info("claimed reminders", "count", len(claimed))
	for _, r := range claimed {
		if err := processReminder(ctx, pool, logger, r); err != nil {
			logger.Error("process reminder failed", "id", r.ID, "err", err)
		}
	}
	return nil
}

func processReminder(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, r db.Reminder) error {
	logger.Info("processing reminder", "id", r.ID, "body", r.Body)
	time.Sleep(20 * time.Second) // TEMP: simulate slow work for crash test
	_, err := pool.Exec(ctx,
		`UPDATE reminders SET status = 'sent', locked_until = NULL WHERE id = $1`,
		r.ID,
	)
	if err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	return nil
}

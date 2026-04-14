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
RETURNING id, telegram_chat_id, body, scheduled_at, recurrence;
`

func claimAndProcess(ctx context.Context, pool *pgxpool.Pool, msgr Messenger, agent ReminderAgent, logger *slog.Logger) error {
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
		if err := rows.Scan(&r.ID, &r.TelegramChatID, &r.Body, &r.ScheduledAt, &r.Recurrence); err != nil {
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
		if err := processReminder(ctx, pool, msgr, agent, logger, r); err != nil {
			logger.Error("process reminder failed", "id", r.ID, "err", err)
		}
	}
	return nil
}

func processReminder(ctx context.Context, pool *pgxpool.Pool, msgr Messenger, agent ReminderAgent, logger *slog.Logger, r db.Reminder) error {
	logger.Info("processing reminder", "id", r.ID, "body", r.Body, "recurrence", r.Recurrence)

	msg, err := agent.Compose(ctx, r.Body)
	if err != nil {
		logger.Warn("reminder agent compose failed, using raw body", "err", err)
		msg = r.Body
	}

	if err := msgr.Send(ctx, r.TelegramChatID, msg); err != nil {
		// Mark failed so we don't spin on a permanently broken reminder.
		_, updErr := pool.Exec(ctx,
			`UPDATE reminders SET status = 'failed', locked_until = NULL WHERE id = $1`,
			r.ID,
		)
		if updErr != nil {
			return fmt.Errorf("send failed and mark-failed failed: send=%w mark=%v", err, updErr)
		}
		return fmt.Errorf("send: %w", err)
	}

	// Recurring reminder: advance scheduled_at to the next occurrence and reset
	// to pending so the scheduler picks it up again. The row is updated in place
	// to avoid unbounded table growth.
	if r.Recurrence != nil && *r.Recurrence != "" {
		next, err := nextOccurrence(r.ScheduledAt, *r.Recurrence)
		if err != nil {
			logger.Error("failed to compute next occurrence — marking sent instead",
				"id", r.ID, "recurrence", *r.Recurrence, "err", err)
		} else {
			_, err = pool.Exec(ctx,
				`UPDATE reminders SET status = 'pending', scheduled_at = $1, locked_until = NULL WHERE id = $2`,
				next, r.ID,
			)
			if err != nil {
				return fmt.Errorf("reschedule recurring reminder: %w", err)
			}
			logger.Info("recurring reminder rescheduled", "id", r.ID, "next", next)
			return nil
		}
	}

	_, err = pool.Exec(ctx,
		`UPDATE reminders SET status = 'sent', locked_until = NULL WHERE id = $1`,
		r.ID,
	)
	if err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	return nil
}

// nextOccurrence returns the next future fire time for a recurring reminder,
// advancing by one interval from scheduledAt until the result is after now.
func nextOccurrence(scheduledAt time.Time, recurrence string) (time.Time, error) {
	now := time.Now()
	next := scheduledAt
	for !next.After(now) {
		switch recurrence {
		case "hourly":
			next = next.Add(time.Hour)
		case "daily":
			next = next.AddDate(0, 0, 1)
		case "weekly":
			next = next.AddDate(0, 0, 7)
		case "monthly":
			next = next.AddDate(0, 1, 0)
		default:
			return time.Time{}, fmt.Errorf("unknown recurrence %q", recurrence)
		}
	}
	return next, nil
}

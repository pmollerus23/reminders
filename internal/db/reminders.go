package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReminderStatus is the lifecycle state of a reminder. The values must
// stay in sync with the CHECK constraint in migration 0001.
type ReminderStatus string

const (
	StatusPending ReminderStatus = "pending"
	StatusSent    ReminderStatus = "sent"
	StatusFailed  ReminderStatus = "failed"
)

// Reminder mirrors a row in the reminders table.
type Reminder struct {
	ID             uuid.UUID
	TelegramChatID int64
	Body           string
	ScheduledAt    time.Time
	Status         ReminderStatus
	LockedUntil    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ErrReminderNotFound is returned when a query expects a row and finds none.
var ErrReminderNotFound = errors.New("reminder not found")

// CreateReminder inserts a new pending reminder and returns the full row,
// including server-assigned fields (id, timestamps, default status).
func CreateReminder(
	ctx context.Context,
	pool *pgxpool.Pool,
	chatID int64,
	body string,
	scheduledAt time.Time,
) (*Reminder, error) {
	const query = `
		INSERT INTO reminders (telegram_chat_id, body, scheduled_at)
		VALUES ($1, $2, $3)
		RETURNING id, telegram_chat_id, body, scheduled_at,
		          status, locked_until, created_at, updated_at
	`

	var r Reminder
	err := pool.QueryRow(ctx, query, chatID, body, scheduledAt).Scan(
		&r.ID,
		&r.TelegramChatID,
		&r.Body,
		&r.ScheduledAt,
		&r.Status,
		&r.LockedUntil,
		&r.CreatedAt,
		&r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("db: create reminder: %w", err)
	}

	return &r, nil
}

// GetDueReminders returns up to `limit` pending reminders whose scheduled
// time has passed and which are not currently locked. Results are ordered
// by scheduled_at ascending so the oldest due reminder is processed first.
//
// Note: this query does NOT lock rows. Locking happens in M4 when the
// scheduler claims work via FOR UPDATE SKIP LOCKED.
func GetDueReminders(
	ctx context.Context,
	pool *pgxpool.Pool,
	limit int32,
) ([]Reminder, error) {
	const query = `
		SELECT id, telegram_chat_id, body, scheduled_at,
		       status, locked_until, created_at, updated_at
		FROM reminders
		WHERE status = 'pending'
		  AND scheduled_at <= now()
		  AND (locked_until IS NULL OR locked_until < now())
		ORDER BY scheduled_at ASC
		LIMIT $1
	`

	rows, err := pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("db: query due reminders: %w", err)
	}
	defer rows.Close()

	var reminders []Reminder
	for rows.Next() {
		var r Reminder
		if err := rows.Scan(
			&r.ID,
			&r.TelegramChatID,
			&r.Body,
			&r.ScheduledAt,
			&r.Status,
			&r.LockedUntil,
			&r.CreatedAt,
			&r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan reminder: %w", err)
		}
		reminders = append(reminders, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate reminders: %w", err)
	}

	return reminders, nil
}

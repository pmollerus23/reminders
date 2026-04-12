package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/reminder"
	"github.com/pmollerus23/reminders/internal/telegram"
)

// Messenger is what the handler needs to send replies. Defined here,
// on the consumer side — same discipline as scheduler.Messenger.
// The two interfaces are intentionally duplicated rather than shared:
// handler and scheduler happen to need the same shape today, but
// coupling them would be premature. If scheduler later needs a second
// method that handler doesn't, the interfaces diverge cleanly.
type Messenger interface {
	Send(ctx context.Context, chatID int64, body string) error
}

// Handler composes parsing + persistence + replying into the inbound flow.
// It implements telegram.Handler structurally — no explicit declaration.
type Handler struct {
	pool   *pgxpool.Pool
	msgr   Messenger
	logger *slog.Logger
}

func New(pool *pgxpool.Pool, msgr Messenger, logger *slog.Logger) *Handler {
	return &Handler{pool: pool, msgr: msgr, logger: logger}
}

const usage = "Usage: /remind YYYY-MM-DD HH:MM <your reminder text>"

// Handle processes one inbound update. Non-command messages are ignored
// silently. Malformed commands get a usage reply. Well-formed commands
// create a reminder and get a confirmation reply.
//
// Errors returned from Handle indicate *infrastructure* failures (DB down,
// Telegram API rejecting a valid send). User mistakes are handled inline
// by replying — they are not errors from the system's perspective.
func (h *Handler) Handle(ctx context.Context, u telegram.Update) error {
	parsed, err := reminder.ParseCommand(u.Text)
	switch {
	case errors.Is(err, reminder.ErrNotACommand):
		return nil // not ours; silently ignore
	case errors.Is(err, reminder.ErrBadFormat):
		return h.reply(ctx, u.ChatID, usage)
	case err != nil:
		// Unknown parse error — log and reply generically.
		h.logger.Error("parse command unexpected error", "err", err)
		return h.reply(ctx, u.ChatID, "Sorry, something went wrong.")
	}

	r, err := db.CreateReminder(ctx, h.pool, u.ChatID, parsed.What, parsed.When)
	if err != nil {
		// DB failure is a real error — return it so it gets logged
		// centrally, and try to tell the user something went wrong.
		_ = h.reply(ctx, u.ChatID, "Sorry, couldn't save your reminder.")
		return fmt.Errorf("handler: create reminder: %w", err)
	}

	h.logger.Info("reminder created", "id", r.ID, "chat_id", u.ChatID, "when", r.ScheduledAt)

	confirm := fmt.Sprintf("Got it — I'll remind you at %s", r.ScheduledAt.Format("2006-01-02 15:04 MST"))
	return h.reply(ctx, u.ChatID, confirm)
}

// reply is a thin wrapper that wraps the error with handler context,
// so DB errors and reply errors are distinguishable in logs.
func (h *Handler) reply(ctx context.Context, chatID int64, body string) error {
	if err := h.msgr.Send(ctx, chatID, body); err != nil {
		return fmt.Errorf("handler: reply: %w", err)
	}
	return nil
}

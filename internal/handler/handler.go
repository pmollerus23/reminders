package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/reminder"
	"github.com/pmollerus23/reminders/internal/telegram"
)

// Messenger is what the handler needs to send replies. Defined here,
// on the consumer side. The scheduler has its own identical interface —
// duplicated deliberately so the two consumers stay decoupled.
type Messenger interface {
	Send(ctx context.Context, chatID int64, body string) error
}

type Handler struct {
	pool   *pgxpool.Pool
	msgr   Messenger
	parser reminder.Parser
	loc    *time.Location
	logger *slog.Logger
}

func New(
	pool *pgxpool.Pool,
	msgr Messenger,
	parser reminder.Parser,
	loc *time.Location,
	logger *slog.Logger,
) *Handler {
	return &Handler{pool: pool, msgr: msgr, parser: parser, loc: loc, logger: logger}
}

const (
	commandPrefix = "/remind"
	usage         = "Usage: /remind <your reminder text>"
)

// Handle processes one inbound update. Non-command messages are ignored
// silently. Parse failures reply with a user-facing message from the
// parser. Infrastructure failures reply with a generic apology.
func (h *Handler) Handle(ctx context.Context, u telegram.Update) error {
	text := strings.TrimSpace(u.Text)

	// Defense in depth: Telegram's library already routes only /remind
	// here, but keeping the check makes the handler predictable when
	// called from tests or other entry points.
	if !strings.HasPrefix(text, commandPrefix) {
		return nil
	}

	body := strings.TrimSpace(strings.TrimPrefix(text, commandPrefix))
	if body == "" {
		return h.reply(ctx, u.ChatID, usage)
	}

	req := reminder.ParseRequest{
		Text: body,
		Now:  time.Now().In(h.loc),
		Loc:  h.loc,
	}
	parsed, err := h.parser.Parse(ctx, req)
	if err != nil {
		var perr *reminder.ParseError
		if errors.As(err, &perr) {
			h.logger.Info("parse rejected",
				"chat_id", u.ChatID,
				"text", body,
				"user_msg", perr.UserMessage,
				"err", err,
			)
			return h.reply(ctx, u.ChatID, perr.UserMessage)
		}
		h.logger.Error("parse infrastructure error", "err", err, "text", body)
		return h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
	}

	r, err := db.CreateReminder(ctx, h.pool, u.ChatID, parsed.What, parsed.When)
	if err != nil {
		_ = h.reply(ctx, u.ChatID, "Sorry, couldn't save your reminder.")
		return fmt.Errorf("handler: create reminder: %w", err)
	}

	h.logger.Info("reminder created", "id", r.ID, "chat_id", u.ChatID, "when", r.ScheduledAt)

	confirm := fmt.Sprintf("Got it — I'll remind you at %s",
		r.ScheduledAt.In(h.loc).Format("2006-01-02 15:04 MST"))
	return h.reply(ctx, u.ChatID, confirm)
}

func (h *Handler) reply(ctx context.Context, chatID int64, body string) error {
	if err := h.msgr.Send(ctx, chatID, body); err != nil {
		return fmt.Errorf("handler: reply: %w", err)
	}
	return nil
}

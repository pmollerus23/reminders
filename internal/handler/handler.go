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
	"github.com/pmollerus23/reminders/internal/intent"
	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
	"github.com/pmollerus23/reminders/internal/telegram"
)

// Messenger is what the handler needs to send replies. Defined here,
// on the consumer side. The scheduler has its own identical interface —
// duplicated deliberately so the two consumers stay decoupled.
type Messenger interface {
	Send(ctx context.Context, chatID int64, body string) error
}

// ChatBuilder assembles an LLM prompt from stored memory. Defined here
// (consumer side) so the handler can be tested without a real store.
type ChatBuilder interface {
	BuildChatPrompt(ctx context.Context, chatID int64, userInput string) (promptctx.Prompt, error)
}

type Handler struct {
	pool        *pgxpool.Pool
	msgr        Messenger
	dispatcher  intent.Dispatcher
	chatBuilder ChatBuilder
	mem         memory.Store
	loc         *time.Location
	logger      *slog.Logger
}

func New(
	pool *pgxpool.Pool,
	msgr Messenger,
	dispatcher intent.Dispatcher,
	mem memory.Store,
	chatBuilder ChatBuilder,
	loc *time.Location,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		pool:        pool,
		msgr:        msgr,
		dispatcher:  dispatcher,
		chatBuilder: chatBuilder,
		mem:         mem,
		loc:         loc,
		logger:      logger,
	}
}

// Handle processes one inbound update. Empty text is silently ignored.
// Unknown slash commands are silently ignored. /jarvis is accepted as a
// backward-compatible prefix; its body is stripped before dispatch so the
// LLM sees only the user's intent. Plain text is dispatched directly.
func (h *Handler) Handle(ctx context.Context, u telegram.Update) error {
	text := strings.TrimSpace(u.Text)
	if text == "" {
		return nil
	}

	// /jarvis is a supported alias — strip the command prefix.
	if text == "/jarvis" {
		return h.reply(ctx, u.ChatID, "What would you like?")
	}
	if after, ok := strings.CutPrefix(text, "/jarvis "); ok {
		text = strings.TrimSpace(after)
	} else if strings.HasPrefix(text, "/") {
		// All other slash commands are silently ignored.
		return nil
	}

	return h.handleUnified(ctx, u, text)
}

func (h *Handler) handleUnified(ctx context.Context, u telegram.Update, text string) error {
	p, err := h.chatBuilder.BuildChatPrompt(ctx, u.ChatID, text)
	if err != nil {
		h.logger.Error("unified: build prompt", "err", err, "chat_id", u.ChatID)
		_ = h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
		return fmt.Errorf("handler: build prompt: %w", err)
	}

	action, err := h.dispatcher.Dispatch(ctx, p, time.Now().In(h.loc), h.loc)
	if err != nil {
		var de *intent.DispatchError
		if errors.As(err, &de) {
			// User-facing rejection (e.g. stub mode, ambiguous input). Persist
			// the exchange so the summary reflects the failed attempt.
			h.replyAndPersist(ctx, u.ChatID, text, de.UserMessage)
			return nil
		}
		h.logger.Error("unified: dispatch", "err", err, "chat_id", u.ChatID)
		_ = h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
		return fmt.Errorf("handler: dispatch: %w", err)
	}

	switch action.Kind {
	case intent.ActionSetReminder:
		return h.actSetReminder(ctx, u, text, action)
	case intent.ActionRememberFact:
		return h.actRememberFact(ctx, u, text, action)
	case intent.ActionChat:
		h.replyAndPersist(ctx, u.ChatID, text, action.ChatReply)
		return nil
	default:
		return fmt.Errorf("handler: unknown action kind %q", action.Kind)
	}
}

func (h *Handler) actSetReminder(ctx context.Context, u telegram.Update, text string, a intent.Action) error {
	when, err := time.Parse(time.RFC3339, a.ReminderWhen)
	if err != nil {
		// The dispatcher validated this; reaching here indicates a bug.
		h.logger.Error("set_reminder: invalid RFC3339", "when", a.ReminderWhen, "err", err)
		return h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
	}

	r, err := db.CreateReminder(ctx, h.pool, u.ChatID, a.ReminderWhat, when, a.ReminderRecurrence)
	if err != nil {
		_ = h.reply(ctx, u.ChatID, "Sorry, couldn't save your reminder.")
		return fmt.Errorf("handler: create reminder: %w", err)
	}

	h.logger.Info("reminder created", "id", r.ID, "chat_id", u.ChatID, "when", r.ScheduledAt, "recurrence", r.Recurrence)
	confirm := fmt.Sprintf("Got it — I'll remind you at %s",
		r.ScheduledAt.In(h.loc).Format("2006-01-02 15:04 MST"))
	if r.Recurrence != nil {
		confirm += fmt.Sprintf(", then %s", *r.Recurrence)
	}
	h.replyAndPersist(ctx, u.ChatID, text, confirm)
	return nil
}

func (h *Handler) actRememberFact(ctx context.Context, u telegram.Update, text string, a intent.Action) error {
	if err := h.mem.AddFact(ctx, u.ChatID, memory.Fact{
		Kind:    a.FactKind,
		Content: a.FactContent,
		Source:  memory.SourceUser,
	}); err != nil {
		_ = h.reply(ctx, u.ChatID, "Sorry, couldn't save that.")
		return fmt.Errorf("handler: add fact: %w", err)
	}
	h.logger.Info("fact stored", "chat_id", u.ChatID, "kind", a.FactKind)
	h.replyAndPersist(ctx, u.ChatID, text, "Got it — I'll remember that.")
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (h *Handler) reply(ctx context.Context, chatID int64, body string) error {
	if err := h.msgr.Send(ctx, chatID, body); err != nil {
		return fmt.Errorf("handler: reply: %w", err)
	}
	return nil
}

// replyAndPersist sends a reply then appends both turns to memory. Memory
// writes are best-effort: errors are logged at Warn and do not propagate.
// This is deliberately different from CreateReminder / AddFact errors, which
// are returned — those operations are the product; memory is a side effect.
func (h *Handler) replyAndPersist(ctx context.Context, chatID int64, userBody, replyText string) {
	if err := h.msgr.Send(ctx, chatID, replyText); err != nil {
		h.logger.Error("handler: reply", "chat_id", chatID, "err", err)
		// Don't persist turns if the send failed — the user never saw the reply.
		return
	}
	if err := h.mem.AppendTurn(ctx, chatID, memory.Turn{Role: memory.RoleUser, Content: userBody}); err != nil {
		h.logger.Warn("handler: persist user turn", "chat_id", chatID, "err", err)
	}
	if err := h.mem.AppendTurn(ctx, chatID, memory.Turn{Role: memory.RoleAssistant, Content: replyText}); err != nil {
		h.logger.Warn("handler: persist assistant turn", "chat_id", chatID, "err", err)
	}
}

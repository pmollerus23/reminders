package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
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

	// Build the ToolSet: closures that capture chatID and handler state.
	// Results are returned to the agentic loop as tool_result content so the
	// model can incorporate them into its final reply.
	tools := intent.ToolSet{
		// ── Reminder write ───────────────────────────────────────────────────

		SetReminder: func(ctx context.Context, what string, when time.Time, recurrence *string) (string, error) {
			r, err := db.CreateReminder(ctx, h.pool, u.ChatID, what, when, recurrence)
			if err != nil {
				return "", fmt.Errorf("create reminder: %w", err)
			}
			h.logger.Info("reminder created", "id", r.ID, "chat_id", u.ChatID, "when", r.ScheduledAt, "recurrence", r.Recurrence)
			msg := fmt.Sprintf("Reminder set for %s", r.ScheduledAt.In(h.loc).Format("2006-01-02 15:04 MST"))
			if r.Recurrence != nil {
				msg += fmt.Sprintf(", then %s", *r.Recurrence)
			}
			return msg, nil
		},

		UpdateReminder: func(ctx context.Context, id string, what string, when time.Time, recurrence *string) (string, error) {
			uid, err := parseUUID(id)
			if err != nil {
				return "", err
			}
			if err := db.UpdateReminder(ctx, h.pool, u.ChatID, uid, what, when, recurrence); err != nil {
				return "", fmt.Errorf("update reminder: %w", err)
			}
			h.logger.Info("reminder updated", "id", id, "chat_id", u.ChatID, "when", when)
			msg := fmt.Sprintf("Reminder updated to %s", when.In(h.loc).Format("2006-01-02 15:04 MST"))
			if recurrence != nil {
				msg += fmt.Sprintf(", then %s", *recurrence)
			}
			return msg, nil
		},

		DeleteReminder: func(ctx context.Context, id string) (string, error) {
			uid, err := parseUUID(id)
			if err != nil {
				return "", err
			}
			if err := db.DeleteReminder(ctx, h.pool, u.ChatID, uid); err != nil {
				return "", fmt.Errorf("delete reminder: %w", err)
			}
			h.logger.Info("reminder deleted", "id", id, "chat_id", u.ChatID)
			return "Reminder deleted.", nil
		},

		// ── Reminder read ────────────────────────────────────────────────────

		ListReminders: func(ctx context.Context) ([]intent.ReminderSummary, error) {
			reminders, err := db.GetPendingReminders(ctx, h.pool, u.ChatID)
			if err != nil {
				return nil, fmt.Errorf("list reminders: %w", err)
			}
			out := make([]intent.ReminderSummary, len(reminders))
			for i, r := range reminders {
				out[i] = intent.ReminderSummary{
					ID:   r.ID.String(),
					What: r.Body,
					When: r.ScheduledAt.In(h.loc).Format(time.RFC3339),
				}
				if r.Recurrence != nil {
					out[i].Recurrence = *r.Recurrence
				}
			}
			return out, nil
		},

		// ── Fact write ───────────────────────────────────────────────────────

		RememberFact: func(ctx context.Context, kind memory.Kind, content json.RawMessage) (string, error) {
			if err := h.mem.AddFact(ctx, u.ChatID, memory.Fact{
				Kind:    kind,
				Content: content,
				Source:  memory.SourceUser,
			}); err != nil {
				return "", fmt.Errorf("add fact: %w", err)
			}
			h.logger.Info("fact stored", "chat_id", u.ChatID, "kind", kind)
			return "Fact stored.", nil
		},

		UpdateFact: func(ctx context.Context, id string, kind memory.Kind, content json.RawMessage) (string, error) {
			uid, err := parseUUID(id)
			if err != nil {
				return "", err
			}
			if err := h.mem.UpdateFact(ctx, u.ChatID, uid, kind, content); err != nil {
				return "", fmt.Errorf("update fact: %w", err)
			}
			h.logger.Info("fact updated", "id", id, "chat_id", u.ChatID, "kind", kind)
			return "Fact updated.", nil
		},

		DeleteFact: func(ctx context.Context, id string) (string, error) {
			uid, err := parseUUID(id)
			if err != nil {
				return "", err
			}
			if err := h.mem.DeleteFact(ctx, u.ChatID, uid); err != nil {
				return "", fmt.Errorf("delete fact: %w", err)
			}
			h.logger.Info("fact deleted", "id", id, "chat_id", u.ChatID)
			return "Fact deleted.", nil
		},

		// ── Fact read ────────────────────────────────────────────────────────

		ListFacts: func(ctx context.Context) ([]intent.FactSummary, error) {
			facts, err := h.mem.Facts(ctx, u.ChatID)
			if err != nil {
				return nil, fmt.Errorf("list facts: %w", err)
			}
			out := make([]intent.FactSummary, len(facts))
			for i, f := range facts {
				out[i] = intent.FactSummary{
					ID:      f.ID.String(),
					Kind:    string(f.Kind),
					Content: f.Content,
				}
			}
			return out, nil
		},
	}

	reply, err := h.dispatcher.Dispatch(ctx, p, time.Now().In(h.loc), h.loc, tools)
	if err != nil {
		var de *intent.DispatchError
		if errors.As(err, &de) {
			// User-facing rejection (e.g. stub mode). Persist the exchange so
			// the summary reflects the failed attempt.
			h.replyAndPersist(ctx, u.ChatID, text, de.UserMessage)
			return nil
		}
		h.logger.Error("unified: dispatch", "err", err, "chat_id", u.ChatID)
		_ = h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
		return fmt.Errorf("handler: dispatch: %w", err)
	}

	h.replyAndPersist(ctx, u.ChatID, text, reply)
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid id %q: %w", s, err)
	}
	return id, nil
}

func (h *Handler) reply(ctx context.Context, chatID int64, body string) error {
	if err := h.msgr.Send(ctx, chatID, body); err != nil {
		return fmt.Errorf("handler: reply: %w", err)
	}
	return nil
}

// replyAndPersist sends a reply then appends both turns to memory. Memory
// writes are best-effort: errors are logged at Warn and do not propagate.
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

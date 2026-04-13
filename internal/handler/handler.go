package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pmollerus23/reminders/internal/chat"
	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/fact"
	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
	"github.com/pmollerus23/reminders/internal/reminder"
	"github.com/pmollerus23/reminders/internal/telegram"
)

// Messenger is what the handler needs to send replies. Defined here,
// on the consumer side. The scheduler has its own identical interface —
// duplicated deliberately so the two consumers stay decoupled.
type Messenger interface {
	Send(ctx context.Context, chatID int64, body string) error
}

// ChatBuilder assembles an LLM prompt from stored memory for the /chat command.
// Defined here (consumer side) so the handler can be tested without a real store.
type ChatBuilder interface {
	BuildChatPrompt(ctx context.Context, chatID int64, userInput string) (promptctx.Prompt, error)
}

type Handler struct {
	pool          *pgxpool.Pool
	msgr          Messenger
	parser        reminder.Parser
	factParser    fact.Parser
	mem           memory.Store
	chatBuilder   ChatBuilder
	chatResponder chat.Responder
	loc           *time.Location
	logger        *slog.Logger
}

func New(
	pool *pgxpool.Pool,
	msgr Messenger,
	parser reminder.Parser,
	factParser fact.Parser,
	mem memory.Store,
	chatBuilder ChatBuilder,
	chatResponder chat.Responder,
	loc *time.Location,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		pool:          pool,
		msgr:          msgr,
		parser:        parser,
		factParser:    factParser,
		mem:           mem,
		chatBuilder:   chatBuilder,
		chatResponder: chatResponder,
		loc:           loc,
		logger:        logger,
	}
}

const (
	jarvisCmd   = "/jarvis"
	rememberCmd = "/remember"
	chatCmd     = "/chat"

	jarvisUsage   = "Usage: /jarvis <your reminder text>"
	rememberUsage = "Usage: /remember <something about you>"
	chatUsage     = "Usage: /chat <message>"
)

// Handle processes one inbound update. Non-command messages are silently
// ignored. Each command is dispatched by its first whitespace-delimited token
// so that /jarvis and /remember are unambiguous regardless of the body text.
func (h *Handler) Handle(ctx context.Context, u telegram.Update) error {
	text := strings.TrimSpace(u.Text)
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}

	switch fields[0] {
	case jarvisCmd:
		return h.handleJarvis(ctx, u, text)
	case rememberCmd:
		return h.handleRemember(ctx, u, text)
	case chatCmd:
		return h.handleChat(ctx, u, text)
	default:
		return nil
	}
}

// ── /jarvis ───────────────────────────────────────────────────────────────────

func (h *Handler) handleJarvis(ctx context.Context, u telegram.Update, text string) error {
	body := commandBody(text, jarvisCmd)
	if body == "" {
		// Usage hint: not a conversation turn, don't persist.
		return h.reply(ctx, u.ChatID, jarvisUsage)
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
			h.logger.Info("reminder parse rejected",
				"chat_id", u.ChatID,
				"text", body,
				"user_msg", perr.UserMessage,
				"err", err,
			)
			// Persist the failed attempt — the summary should reflect that the
			// user tried to interact, not just successful flows.
			h.replyAndPersist(ctx, u.ChatID, body, perr.UserMessage)
			return nil
		}
		h.logger.Error("reminder parse infrastructure error", "err", err, "text", body)
		return h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
	}

	r, err := db.CreateReminder(ctx, h.pool, u.ChatID, parsed.What, parsed.When)
	if err != nil {
		// Memory write is intentionally skipped here: the reminder wasn't saved,
		// so there is no meaningful exchange to record.
		_ = h.reply(ctx, u.ChatID, "Sorry, couldn't save your reminder.")
		return fmt.Errorf("handler: create reminder: %w", err)
	}

	h.logger.Info("reminder created", "id", r.ID, "chat_id", u.ChatID, "when", r.ScheduledAt)

	confirm := fmt.Sprintf("Got it — I'll remind you at %s",
		r.ScheduledAt.In(h.loc).Format("2006-01-02 15:04 MST"))
	h.replyAndPersist(ctx, u.ChatID, body, confirm)
	return nil
}

// ── /remember ─────────────────────────────────────────────────────────────────

func (h *Handler) handleRemember(ctx context.Context, u telegram.Update, text string) error {
	body := commandBody(text, rememberCmd)
	if body == "" {
		// Usage hint: not a conversation turn, don't persist.
		return h.reply(ctx, u.ChatID, rememberUsage)
	}

	parsed, err := h.factParser.Parse(ctx, fact.ParseRequest{Text: body})
	if err != nil {
		var perr *fact.ParseError
		if errors.As(err, &perr) {
			h.logger.Info("fact parse rejected",
				"chat_id", u.ChatID,
				"text", body,
				"user_msg", perr.UserMessage,
				"err", err,
			)
			h.replyAndPersist(ctx, u.ChatID, body, perr.UserMessage)
			return nil
		}
		h.logger.Error("fact parse infrastructure error", "err", err, "text", body)
		return h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
	}

	if err := h.mem.AddFact(ctx, u.ChatID, memory.Fact{
		Kind:    parsed.Kind,
		Content: parsed.Content,
		Source:  memory.SourceUser,
	}); err != nil {
		// AddFact is an infrastructure operation — fail visibly so the user
		// can retry, unlike AppendTurn which is best-effort.
		_ = h.reply(ctx, u.ChatID, "Sorry, couldn't save that.")
		return fmt.Errorf("handler: add fact: %w", err)
	}

	h.logger.Info("fact stored", "chat_id", u.ChatID, "kind", parsed.Kind)

	const confirmMsg = "Got it — I'll remember that."
	h.replyAndPersist(ctx, u.ChatID, body, confirmMsg)
	return nil
}

// ── /chat ─────────────────────────────────────────────────────────────────────

func (h *Handler) handleChat(ctx context.Context, u telegram.Update, text string) error {
	body := commandBody(text, chatCmd)
	if body == "" {
		// Usage hint: not a conversation turn, don't persist.
		return h.reply(ctx, u.ChatID, chatUsage)
	}

	p, err := h.chatBuilder.BuildChatPrompt(ctx, u.ChatID, body)
	if err != nil {
		h.logger.Error("chat: build prompt", "err", err, "chat_id", u.ChatID)
		_ = h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
		return fmt.Errorf("handler: chat build prompt: %w", err)
	}

	replyText, err := h.chatResponder.Respond(ctx, p)
	if err != nil {
		var rerr *chat.RespondError
		if errors.As(err, &rerr) {
			// Expected constraint (e.g. PARSER=regex stub) — user message is safe.
			return h.reply(ctx, u.ChatID, rerr.UserMessage)
		}
		h.logger.Error("chat: respond", "err", err, "chat_id", u.ChatID)
		_ = h.reply(ctx, u.ChatID, "Sorry, something went wrong. Please try again.")
		return fmt.Errorf("handler: chat respond: %w", err)
	}

	h.replyAndPersist(ctx, u.ChatID, body, replyText)
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// commandBody strips the command token and returns the remaining body trimmed.
// Returns "" when the body is empty.
func commandBody(text, cmd string) string {
	return strings.TrimSpace(strings.TrimPrefix(text, cmd))
}

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

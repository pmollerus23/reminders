package handler_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pmollerus23/reminders/internal/chat"
	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/fact"
	"github.com/pmollerus23/reminders/internal/handler"
	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
	"github.com/pmollerus23/reminders/internal/reminder"
	"github.com/pmollerus23/reminders/internal/telegram"
)

// ── Fakes ─────────────────────────────────────────────────────────────────────

type fakeMessenger struct {
	sent []string
	err  error
}

func (f *fakeMessenger) Send(_ context.Context, _ int64, body string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, body)
	return nil
}

type fakeReminderParser struct {
	result reminder.ParsedReminder
	err    error
}

func (f *fakeReminderParser) Parse(_ context.Context, _ reminder.ParseRequest) (reminder.ParsedReminder, error) {
	return f.result, f.err
}

type fakeFactParser struct {
	result fact.ParsedFact
	err    error
}

func (f *fakeFactParser) Parse(_ context.Context, _ fact.ParseRequest) (fact.ParsedFact, error) {
	return f.result, f.err
}

// fakeMemStore implements memory.Store for handler tests.
// Only AppendTurn and AddFact are called by the handler. The embedded nil
// interface satisfies the remaining methods at compile time; any accidental
// call will panic immediately, which is the correct signal.
type fakeMemStore struct {
	memory.Store // nil embed; unimplemented calls panic
	turns        []memory.Turn
	facts        []memory.Fact
	appendErr    error
	addFactErr   error
}

func (f *fakeMemStore) AppendTurn(_ context.Context, _ int64, t memory.Turn) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.turns = append(f.turns, t)
	return nil
}

func (f *fakeMemStore) AddFact(_ context.Context, _ int64, fct memory.Fact) error {
	if f.addFactErr != nil {
		return f.addFactErr
	}
	f.facts = append(f.facts, fct)
	return nil
}

// fakeChatBuilder implements handler.ChatBuilder for tests.
type fakeChatBuilder struct {
	prompt promptctx.Prompt
	err    error
}

func (f *fakeChatBuilder) BuildChatPrompt(_ context.Context, _ int64, _ string) (promptctx.Prompt, error) {
	return f.prompt, f.err
}

// fakeChatResponder implements chat.Responder for tests.
type fakeChatResponder struct {
	reply string
	err   error
}

func (f *fakeChatResponder) Respond(_ context.Context, _ promptctx.Prompt) (string, error) {
	return f.reply, f.err
}

// discardLogger is a no-op logger used in tests to suppress output.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// newHandler builds a Handler wired with fakes. pool may be nil for tests that
// don't reach db.CreateReminder.
func newHandler(
	pool *pgxpool.Pool,
	msgr *fakeMessenger,
	rp *fakeReminderParser,
	fp *fakeFactParser,
	mem *fakeMemStore,
	cb *fakeChatBuilder,
	cr *fakeChatResponder,
) *handler.Handler {
	return handler.New(pool, msgr, rp, fp, mem, cb, cr, time.UTC, discardLogger)
}

// openTestPool opens a real DB pool for integration tests. Skips when
// TEST_DATABASE_URL is unset, matching the pattern in memory_test.go.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration tests")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// ── /remember tests ───────────────────────────────────────────────────────────

func TestRemember_Success(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	fp := &fakeFactParser{
		result: fact.ParsedFact{
			Kind:    memory.KindPreference,
			Content: []byte(`{"topic":"coffee","detail":"black"}`),
		},
	}

	h := newHandler(nil, msgr, &fakeReminderParser{}, fp, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/remember I love black coffee"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Got it — I'll remember that." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.facts) != 1 || mem.facts[0].Kind != memory.KindPreference {
		t.Errorf("facts: %v", mem.facts)
	}
	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns persisted, got %d", len(mem.turns))
	}
	if mem.turns[0].Role != memory.RoleUser || mem.turns[1].Role != memory.RoleAssistant {
		t.Errorf("turn roles: %v %v", mem.turns[0].Role, mem.turns[1].Role)
	}
}

func TestRemember_FactSourceIsUser(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	fp := &fakeFactParser{
		result: fact.ParsedFact{
			Kind:    memory.KindPerson,
			Content: []byte(`{"name":"Bob","relation":"brother"}`),
		},
	}

	h := newHandler(nil, msgr, &fakeReminderParser{}, fp, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	_ = h.Handle(context.Background(), telegram.Update{ChatID: 1, Text: "/remember my brother Bob"})

	if len(mem.facts) != 1 || mem.facts[0].Source != memory.SourceUser {
		t.Errorf("fact source: want user, got %s", mem.facts[0].Source)
	}
}

func TestRemember_ParseError(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	fp := &fakeFactParser{
		err: &fact.ParseError{UserMessage: "That doesn't look like a personal fact."},
	}

	h := newHandler(nil, msgr, &fakeReminderParser{}, fp, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/remember 2+2=4"}); err != nil {
		t.Fatalf("Handle must not return error on parse rejection, got: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "That doesn't look like a personal fact." {
		t.Errorf("sent: %v", msgr.sent)
	}
	// Turns are persisted even on rejection — summary needs full interaction history.
	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns on parse rejection, got %d", len(mem.turns))
	}
}

func TestRemember_InfraError_AddFact(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{addFactErr: errors.New("db down")}
	fp := &fakeFactParser{
		result: fact.ParsedFact{
			Kind:    memory.KindGoal,
			Content: []byte(`{"description":"exercise","cadence":"daily"}`),
		},
	}

	h := newHandler(nil, msgr, &fakeReminderParser{}, fp, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/remember I want to exercise daily"})
	if err == nil {
		t.Fatal("want error returned for AddFact failure, got nil")
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Sorry, couldn't save that." {
		t.Errorf("sent: %v", msgr.sent)
	}
	// No turns: the handler bails out before the persist step when AddFact fails.
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns on AddFact infra error, got %d", len(mem.turns))
	}
}

func TestRemember_InfraError_Parser(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	fp := &fakeFactParser{err: fmt.Errorf("timeout")} // not a *ParseError

	h := newHandler(nil, msgr, &fakeReminderParser{}, fp, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/remember something"}); err != nil {
		t.Fatalf("Handle should not propagate non-ParseError to caller, got: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Sorry, something went wrong. Please try again." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns on parser infra error, got %d", len(mem.turns))
	}
}

func TestRemember_EmptyBody(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/remember"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(msgr.sent) != 1 {
		t.Fatalf("want usage message, got %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns for usage reply, got %d", len(mem.turns))
	}
}

// ── /jarvis turn-persistence tests ───────────────────────────────────────────

func TestJarvis_TurnsPersisted_OnSuccess(t *testing.T) {
	// Reaches db.CreateReminder — needs a real DB.
	pool := openTestPool(t)
	defer func() {
		// Clean up the reminder row created by this test.
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM reminders WHERE telegram_chat_id = $1", int64(-42))
	}()

	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	rp := &fakeReminderParser{
		result: reminder.ParsedReminder{
			What: "call dentist",
			When: time.Now().Add(24 * time.Hour),
		},
	}

	h := newHandler(pool, msgr, rp, &fakeFactParser{}, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: -42, Text: "/jarvis call dentist tomorrow"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns after successful reminder, got %d", len(mem.turns))
	}
}

func TestJarvis_TurnsPersisted_OnParseRejection(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	rp := &fakeReminderParser{
		err: &reminder.ParseError{UserMessage: "I couldn't figure out when you mean."},
	}

	h := newHandler(nil, msgr, rp, &fakeFactParser{}, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/jarvis blah blah"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns on parse rejection, got %d", len(mem.turns))
	}
}

func TestJarvis_NoTurns_OnEmptyBody(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/jarvis"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns for usage reply, got %d", len(mem.turns))
	}
}

// ── /chat tests ───────────────────────────────────────────────────────────────

func TestChat_Success(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	cb := &fakeChatBuilder{}
	cr := &fakeChatResponder{reply: "Here's what I think!"}

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, cb, cr)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/chat tell me something"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Here's what I think!" {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns after chat, got %d", len(mem.turns))
	}
	if mem.turns[0].Role != memory.RoleUser || mem.turns[1].Role != memory.RoleAssistant {
		t.Errorf("turn roles: %v %v", mem.turns[0].Role, mem.turns[1].Role)
	}
	if mem.turns[0].Content != "tell me something" {
		t.Errorf("user turn content: want %q, got %q", "tell me something", mem.turns[0].Content)
	}
}

func TestChat_EmptyBody(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/chat"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Usage: /chat <message>" {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns for usage reply, got %d", len(mem.turns))
	}
}

func TestChat_StubResponder(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	cr := &fakeChatResponder{
		err: &chat.RespondError{UserMessage: "The /chat command requires PARSER=claude."},
	}

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, &fakeChatBuilder{}, cr)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/chat hello"}); err != nil {
		t.Fatalf("Handle must not return error for RespondError, got: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "The /chat command requires PARSER=claude." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns for stub rejection, got %d", len(mem.turns))
	}
}

func TestChat_InfraError_Respond(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	cr := &fakeChatResponder{err: fmt.Errorf("api timeout")} // not a *RespondError

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, &fakeChatBuilder{}, cr)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/chat hello"}); err == nil {
		t.Fatal("want error returned for Respond infra failure, got nil")
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Sorry, something went wrong. Please try again." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns on infra error, got %d", len(mem.turns))
	}
}

func TestChat_InfraError_BuildPrompt(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	cb := &fakeChatBuilder{err: fmt.Errorf("db down")}

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, cb, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/chat hello"}); err == nil {
		t.Fatal("want error returned for BuildChatPrompt failure, got nil")
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Sorry, something went wrong. Please try again." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns on build prompt error, got %d", len(mem.turns))
	}
}

// ── Routing tests ─────────────────────────────────────────────────────────────

func TestHandle_UnknownCommand_Ignored(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/start"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(msgr.sent) != 0 {
		t.Errorf("want no reply for unknown command, got %v", msgr.sent)
	}
}

func TestHandle_EmptyText_Ignored(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}

	h := newHandler(nil, msgr, &fakeReminderParser{}, &fakeFactParser{}, mem, &fakeChatBuilder{}, &fakeChatResponder{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "   "}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(msgr.sent) != 0 {
		t.Errorf("want no reply for empty text, got %v", msgr.sent)
	}
}

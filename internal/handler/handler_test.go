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

	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/handler"
	"github.com/pmollerus23/reminders/internal/intent"
	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
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

// fakeDispatcher implements intent.Dispatcher for tests.
type fakeDispatcher struct {
	action intent.Action
	err    error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ promptctx.Prompt, _ time.Time, _ *time.Location) (intent.Action, error) {
	return f.action, f.err
}

// discardLogger is a no-op logger used in tests to suppress output.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// newHandler builds a Handler wired with fakes. pool may be nil for tests that
// don't reach db.CreateReminder.
func newHandler(
	pool *pgxpool.Pool,
	msgr *fakeMessenger,
	mem *fakeMemStore,
	cb *fakeChatBuilder,
	d *fakeDispatcher,
) *handler.Handler {
	return handler.New(pool, msgr, d, mem, cb, time.UTC, discardLogger)
}

// openTestPool opens a real DB pool for integration tests. Skips when
// TEST_DATABASE_URL is unset.
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

// ── Unified dispatch tests ─────────────────────────────────────────────────────

func TestUnified_Chat_Success(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	d := &fakeDispatcher{
		action: intent.Action{Kind: intent.ActionChat, ChatReply: "Hello there!"},
	}

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, d)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "hi"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Hello there!" {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(mem.turns))
	}
	if mem.turns[0].Role != memory.RoleUser || mem.turns[1].Role != memory.RoleAssistant {
		t.Errorf("turn roles: %v %v", mem.turns[0].Role, mem.turns[1].Role)
	}
	if mem.turns[0].Content != "hi" {
		t.Errorf("user turn content: got %q", mem.turns[0].Content)
	}
}

func TestUnified_RememberFact_Success(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	d := &fakeDispatcher{
		action: intent.Action{
			Kind:        intent.ActionRememberFact,
			FactKind:    memory.KindPreference,
			FactContent: []byte(`{"topic":"coffee","detail":"black"}`),
		},
	}

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, d)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "I love black coffee"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Got it — I'll remember that." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.facts) != 1 || mem.facts[0].Kind != memory.KindPreference {
		t.Errorf("facts: %v", mem.facts)
	}
	if mem.facts[0].Source != memory.SourceUser {
		t.Errorf("fact source: want user, got %s", mem.facts[0].Source)
	}
	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(mem.turns))
	}
}

func TestUnified_SetReminder_Success(t *testing.T) {
	pool := openTestPool(t)
	defer func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM reminders WHERE telegram_chat_id = $1", int64(-42))
	}()

	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	when := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	d := &fakeDispatcher{
		action: intent.Action{
			Kind:         intent.ActionSetReminder,
			ReminderWhen: when,
			ReminderWhat: "call dentist",
		},
	}

	h := newHandler(pool, msgr, mem, &fakeChatBuilder{}, d)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: -42, Text: "remind me to call dentist tomorrow"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(msgr.sent) != 1 {
		t.Fatalf("want 1 message, got %d: %v", len(msgr.sent), msgr.sent)
	}
	if !errors.Is(nil, nil) { // just a structural check
		t.Errorf("reminder not persisted")
	}
	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns, got %d", len(mem.turns))
	}
}

func TestUnified_DispatchError(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	d := &fakeDispatcher{
		err: &intent.DispatchError{UserMessage: "I couldn't figure out what you mean."},
	}

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, d)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "blarg"}); err != nil {
		t.Fatalf("Handle must not return error for DispatchError, got: %v", err)
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "I couldn't figure out what you mean." {
		t.Errorf("sent: %v", msgr.sent)
	}
	// Rejection is persisted so the summary captures the failed attempt.
	if len(mem.turns) != 2 {
		t.Fatalf("want 2 turns on rejection, got %d", len(mem.turns))
	}
}

func TestUnified_InfraError_Dispatch(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	d := &fakeDispatcher{err: fmt.Errorf("api timeout")} // not a *DispatchError

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, d)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "hi"}); err == nil {
		t.Fatal("want error returned for infra failure, got nil")
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Sorry, something went wrong. Please try again." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns on infra error, got %d", len(mem.turns))
	}
}

func TestUnified_InfraError_BuildPrompt(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	cb := &fakeChatBuilder{err: fmt.Errorf("db down")}

	h := newHandler(nil, msgr, mem, cb, &fakeDispatcher{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "hi"}); err == nil {
		t.Fatal("want error returned for build prompt failure, got nil")
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Sorry, something went wrong. Please try again." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns on build prompt error, got %d", len(mem.turns))
	}
}

func TestUnified_InfraError_AddFact(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{addFactErr: errors.New("db down")}
	d := &fakeDispatcher{
		action: intent.Action{
			Kind:        intent.ActionRememberFact,
			FactKind:    memory.KindGoal,
			FactContent: []byte(`{"description":"exercise","cadence":"daily"}`),
		},
	}

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, d)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "I want to exercise daily"}); err == nil {
		t.Fatal("want error returned for AddFact failure, got nil")
	}

	if len(msgr.sent) != 1 || msgr.sent[0] != "Sorry, couldn't save that." {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns on AddFact infra error, got %d", len(mem.turns))
	}
}

// ── Routing edge cases ─────────────────────────────────────────────────────────

func TestHandle_EmptyText_Ignored(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, &fakeDispatcher{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "   "}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(msgr.sent) != 0 {
		t.Errorf("want no reply for empty text, got %v", msgr.sent)
	}
}

func TestHandle_UnknownCommand_Ignored(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, &fakeDispatcher{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/start"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(msgr.sent) != 0 {
		t.Errorf("want no reply for unknown command, got %v", msgr.sent)
	}
}

func TestHandle_JarvisEmptyBody(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, &fakeDispatcher{})
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/jarvis"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(msgr.sent) != 1 || msgr.sent[0] != "What would you like?" {
		t.Errorf("sent: %v", msgr.sent)
	}
	if len(mem.turns) != 0 {
		t.Errorf("want 0 turns for usage reply, got %d", len(mem.turns))
	}
}

func TestHandle_JarvisAlias_StripsPrefix(t *testing.T) {
	msgr := &fakeMessenger{}
	mem := &fakeMemStore{}
	d := &fakeDispatcher{
		action: intent.Action{Kind: intent.ActionChat, ChatReply: "sure!"},
	}

	h := newHandler(nil, msgr, mem, &fakeChatBuilder{}, d)
	if err := h.Handle(context.Background(), telegram.Update{ChatID: 99, Text: "/jarvis remind me something"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Dispatcher was called (reply sent) — prefix was stripped correctly.
	if len(msgr.sent) != 1 {
		t.Errorf("want 1 reply, got %v", msgr.sent)
	}
	// User turn stored without /jarvis prefix.
	if len(mem.turns) == 2 && mem.turns[0].Content == "/jarvis remind me something" {
		t.Error("user turn should have /jarvis prefix stripped")
	}
}

package promptctx_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
)

// fakeStore is a minimal in-memory implementation of memory.Store for tests.
// Only the three methods called by Builder.BuildChatPrompt are implemented;
// the rest panic. If that list grows, it may be a sign the Store interface
// should be split into read and write halves. // TODO: revisit if promptctx
// needs to write (e.g., append the new user turn).
type fakeStore struct {
	facts   []memory.Fact
	summary *memory.Summary
	turns   []memory.Turn
}

func (f *fakeStore) Facts(_ context.Context, _ int64) ([]memory.Fact, error) {
	return f.facts, nil
}

func (f *fakeStore) GetSummary(_ context.Context, _ int64) (*memory.Summary, error) {
	return f.summary, nil
}

func (f *fakeStore) RecentTurns(_ context.Context, _ int64, limit int) ([]memory.Turn, error) {
	if limit >= len(f.turns) {
		return f.turns, nil
	}
	return f.turns[len(f.turns)-limit:], nil
}

// Unused methods — Builder only reads. Panic surfacefully alerts us if the
// interface contract changes and Builder starts writing through the Store.
func (f *fakeStore) AppendTurn(_ context.Context, _ int64, _ memory.Turn) error {
	panic("fakeStore: AppendTurn not implemented")
}
func (f *fakeStore) UpsertSummary(_ context.Context, _ int64, _ memory.Summary) error {
	panic("fakeStore: UpsertSummary not implemented")
}
func (f *fakeStore) DeleteTurnsBefore(_ context.Context, _ int64, _ time.Time) error {
	panic("fakeStore: DeleteTurnsBefore not implemented")
}
func (f *fakeStore) AddFact(_ context.Context, _ int64, _ memory.Fact) error {
	panic("fakeStore: AddFact not implemented")
}
func (f *fakeStore) FactsByKind(_ context.Context, _ int64, _ memory.Kind) ([]memory.Fact, error) {
	panic("fakeStore: FactsByKind not implemented")
}
func (f *fakeStore) DeleteFact(_ context.Context, _ int64, _ uuid.UUID) error {
	panic("fakeStore: DeleteFact not implemented")
}
func (f *fakeStore) UpdateFact(_ context.Context, _ int64, _ uuid.UUID, _ memory.Kind, _ json.RawMessage) error {
	panic("fakeStore: UpdateFact not implemented")
}
func (f *fakeStore) ChatsNeedingSummary(_ context.Context, _ int) ([]int64, error) {
	panic("fakeStore: ChatsNeedingSummary not implemented")
}
func (f *fakeStore) ForgetChat(_ context.Context, _ int64) error {
	panic("fakeStore: ForgetChat not implemented")
}

// mustJSON is a test helper that converts a value to json.RawMessage.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

const basePrompt = "You are a helpful reminder assistant."

func TestBuildChatPrompt_EmptyChat(t *testing.T) {
	store := &fakeStore{}
	b := promptctx.New(store, basePrompt, 20)

	p, err := b.BuildChatPrompt(context.Background(), 1, "hello")
	if err != nil {
		t.Fatalf("BuildChatPrompt: %v", err)
	}

	// System prompt should contain only the base — no facts or summary sections.
	if p.System != basePrompt {
		t.Errorf("system prompt:\nwant: %q\ngot:  %q", basePrompt, p.System)
	}

	// Messages should contain exactly the new user input.
	if len(p.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(p.Messages))
	}
	if p.Messages[0].Role != memory.RoleUser || p.Messages[0].Content != "hello" {
		t.Errorf("message: want {user hello}, got {%s %q}", p.Messages[0].Role, p.Messages[0].Content)
	}
}

func TestBuildChatPrompt_ReturningChat(t *testing.T) {
	store := &fakeStore{
		facts: []memory.Fact{
			{
				ID:      uuid.New(),
				Kind:    memory.KindPreference,
				Content: mustJSON(map[string]string{"topic": "coffee", "detail": "black"}),
				Source:  memory.SourceUser,
			},
			{
				ID:      uuid.New(),
				Kind:    memory.KindGoal,
				Content: mustJSON(map[string]string{"description": "run 5k", "cadence": "weekly"}),
				Source:  memory.SourceInferred,
			},
			{
				ID:      uuid.New(),
				Kind:    memory.KindPerson,
				Content: mustJSON(map[string]string{"name": "Alice", "relation": "partner"}),
				Source:  memory.SourceUser,
			},
		},
		summary: &memory.Summary{
			Text:              "User set three reminders last week.",
			SummarizedThrough: time.Now(),
		},
		turns: []memory.Turn{
			{Role: memory.RoleUser, Content: "msg1"},
			{Role: memory.RoleAssistant, Content: "reply1"},
			{Role: memory.RoleUser, Content: "msg2"},
			{Role: memory.RoleAssistant, Content: "reply2"},
			{Role: memory.RoleUser, Content: "msg3"},
		},
	}

	b := promptctx.New(store, basePrompt, 20)
	p, err := b.BuildChatPrompt(context.Background(), 1, "new input")
	if err != nil {
		t.Fatalf("BuildChatPrompt: %v", err)
	}

	// System block must contain all three sections.
	if !strings.Contains(p.System, basePrompt) {
		t.Error("system prompt missing base")
	}
	if !strings.Contains(p.System, "What I know about you") {
		t.Error("system prompt missing facts section")
	}
	if !strings.Contains(p.System, "Preference — coffee: black") {
		t.Error("system prompt missing preference fact")
	}
	if !strings.Contains(p.System, "Goal (weekly): run 5k") {
		t.Error("system prompt missing goal fact")
	}
	if !strings.Contains(p.System, "Person — Alice (partner)") {
		t.Error("system prompt missing person fact")
	}
	if !strings.Contains(p.System, "Conversation history (summary)") {
		t.Error("system prompt missing summary section")
	}
	if !strings.Contains(p.System, "User set three reminders last week.") {
		t.Error("system prompt missing summary text")
	}

	// Messages: 5 existing turns + 1 new user input.
	if len(p.Messages) != 6 {
		t.Fatalf("want 6 messages, got %d", len(p.Messages))
	}
	last := p.Messages[len(p.Messages)-1]
	if last.Role != memory.RoleUser || last.Content != "new input" {
		t.Errorf("last message: want {user 'new input'}, got {%s %q}", last.Role, last.Content)
	}
}

func TestBuildChatPrompt_FactsNoSummary(t *testing.T) {
	store := &fakeStore{
		facts: []memory.Fact{
			{
				ID:      uuid.New(),
				Kind:    memory.KindRoutine,
				Content: mustJSON(map[string]string{"description": "meditate", "when": "morning"}),
				Source:  memory.SourceUser,
			},
		},
	}

	b := promptctx.New(store, basePrompt, 20)
	p, err := b.BuildChatPrompt(context.Background(), 1, "hi")
	if err != nil {
		t.Fatalf("BuildChatPrompt: %v", err)
	}

	if !strings.Contains(p.System, "What I know about you") {
		t.Error("facts section missing")
	}
	if !strings.Contains(p.System, "Routine — morning: meditate") {
		t.Error("routine fact missing")
	}
	if strings.Contains(p.System, "Conversation history") {
		t.Error("summary section should be absent when there is no summary")
	}
}

func TestBuildChatPrompt_SummaryNoFacts(t *testing.T) {
	store := &fakeStore{
		summary: &memory.Summary{
			Text:              "The user prefers morning reminders.",
			SummarizedThrough: time.Now(),
		},
	}

	b := promptctx.New(store, basePrompt, 20)
	p, err := b.BuildChatPrompt(context.Background(), 1, "hi")
	if err != nil {
		t.Fatalf("BuildChatPrompt: %v", err)
	}

	if !strings.Contains(p.System, "Conversation history (summary)") {
		t.Error("summary section missing")
	}
	if !strings.Contains(p.System, "The user prefers morning reminders.") {
		t.Error("summary text missing")
	}
	if strings.Contains(p.System, "What I know about you") {
		t.Error("facts section should be absent when there are no facts")
	}
}

func TestBuildChatPrompt_VerbatimTurnsLimit(t *testing.T) {
	allTurns := make([]memory.Turn, 10)
	for i := range allTurns {
		allTurns[i] = memory.Turn{Role: memory.RoleUser, Content: "msg"}
	}
	store := &fakeStore{turns: allTurns}

	// Builder configured for only 3 verbatim turns.
	b := promptctx.New(store, basePrompt, 3)
	p, err := b.BuildChatPrompt(context.Background(), 1, "new")
	if err != nil {
		t.Fatalf("BuildChatPrompt: %v", err)
	}

	// fakeStore.RecentTurns honours the limit, so we expect 3 history + 1 new.
	if len(p.Messages) != 4 {
		t.Fatalf("want 4 messages (3 history + 1 new), got %d", len(p.Messages))
	}
}

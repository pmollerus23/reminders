package summarize

// White-box tests: same package so tick() is accessible without exporting it.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pmollerus23/reminders/internal/memory"
)

// ── Fake store ────────────────────────────────────────────────────────────────

// fakeStore is a thread-safe in-memory Store for testing the summarize loop.
// Only the methods actually called by the Loop are implemented; the rest panic.
type fakeStore struct {
	mu      sync.Mutex
	turns   map[int64][]memory.Turn   // chatID → turns (chronological order)
	summary map[int64]*memory.Summary // chatID → summary
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		turns:   make(map[int64][]memory.Turn),
		summary: make(map[int64]*memory.Summary),
	}
}

func (s *fakeStore) addTurns(chatID int64, turns []memory.Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns[chatID] = append(s.turns[chatID], turns...)
}

func (s *fakeStore) RecentTurns(_ context.Context, chatID int64, limit int) ([]memory.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.turns[chatID]
	if limit >= len(all) {
		cp := make([]memory.Turn, len(all))
		copy(cp, all)
		return cp, nil
	}
	cp := make([]memory.Turn, limit)
	copy(cp, all[len(all)-limit:])
	return cp, nil
}

func (s *fakeStore) ChatsNeedingSummary(_ context.Context, window int) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for id, turns := range s.turns {
		if len(turns) > window {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *fakeStore) GetSummary(_ context.Context, chatID int64) (*memory.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summary[chatID], nil
}

func (s *fakeStore) UpsertSummary(_ context.Context, chatID int64, sum memory.Summary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := sum
	s.summary[chatID] = &cp
	return nil
}

func (s *fakeStore) DeleteTurnsBefore(_ context.Context, chatID int64, cutoff time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var remaining []memory.Turn
	for _, t := range s.turns[chatID] {
		if t.CreatedAt.After(cutoff) {
			remaining = append(remaining, t)
		}
	}
	s.turns[chatID] = remaining
	return nil
}

// Unimplemented — panics loudly if the Loop accidentally calls them.
func (s *fakeStore) AppendTurn(_ context.Context, _ int64, _ memory.Turn) error {
	panic("fakeStore: AppendTurn not called by Loop")
}
func (s *fakeStore) ForgetChat(_ context.Context, _ int64) error {
	panic("fakeStore: ForgetChat not called by Loop")
}
func (s *fakeStore) AddFact(_ context.Context, _ int64, _ memory.Fact) error {
	panic("fakeStore: AddFact not called by Loop")
}
func (s *fakeStore) Facts(_ context.Context, _ int64) ([]memory.Fact, error) {
	panic("fakeStore: Facts not called by Loop")
}
func (s *fakeStore) FactsByKind(_ context.Context, _ int64, _ memory.Kind) ([]memory.Fact, error) {
	panic("fakeStore: FactsByKind not called by Loop")
}
func (s *fakeStore) DeleteFact(_ context.Context, _ int64, _ uuid.UUID) error {
	panic("fakeStore: DeleteFact not called by Loop")
}

// ── Fake summarizer ───────────────────────────────────────────────────────────

type fakeSummarizer struct {
	mu       sync.Mutex
	calls    []summarizeCall
	returnFn func(existing string, turns []memory.Turn) (string, error)
}

type summarizeCall struct {
	existing string
	turns    []memory.Turn
}

func (f *fakeSummarizer) Summarize(_ context.Context, existing string, turns []memory.Turn) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, summarizeCall{existing: existing, turns: turns})
	f.mu.Unlock()
	return f.returnFn(existing, turns)
}

func (f *fakeSummarizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSummarizer) lastCall() summarizeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

// ── Helper ────────────────────────────────────────────────────────────────────

// makeTurns returns n turns with distinct, monotonically-increasing CreatedAt
// timestamps so DeleteTurnsBefore produces predictable results.
func makeTurns(n int) []memory.Turn {
	base := time.Now().UTC().Add(-time.Duration(n) * time.Second)
	turns := make([]memory.Turn, n)
	for i := range turns {
		turns[i] = memory.Turn{
			Role:      memory.RoleUser,
			Content:   "msg",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
	}
	return turns
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestTick_SummarizesOldTurns verifies the happy path:
// old turns are compressed, summary is upserted, old turns are deleted.
func TestTick_SummarizesOldTurns(t *testing.T) {
	const window = 2
	chatID := int64(1001)

	store := newFakeStore()
	store.addTurns(chatID, makeTurns(5)) // 5 turns, window=2 → 3 should be folded

	sum := &fakeSummarizer{returnFn: func(existing string, turns []memory.Turn) (string, error) {
		return "compressed: " + existing, nil
	}}

	loop := NewLoop(store, sum, window, time.Hour, slog.Default())
	loop.tick(context.Background())

	if sum.callCount() != 1 {
		t.Fatalf("want 1 Summarize call, got %d", sum.callCount())
	}
	call := sum.lastCall()
	if call.existing != "" {
		t.Errorf("existing summary should be empty on first call, got %q", call.existing)
	}
	if len(call.turns) != 3 {
		t.Errorf("want 3 turns passed to Summarize (5−2), got %d", len(call.turns))
	}

	// Summary must be stored.
	ctx := context.Background()
	stored, err := store.GetSummary(ctx, chatID)
	if err != nil || stored == nil {
		t.Fatalf("summary not stored: err=%v stored=%v", err, stored)
	}
	if stored.Text != "compressed: " {
		t.Errorf("summary text: got %q", stored.Text)
	}

	// Only 2 (window) turns should remain.
	remaining, _ := store.RecentTurns(ctx, chatID, 1000)
	if len(remaining) != window {
		t.Errorf("want %d turns remaining, got %d", window, len(remaining))
	}
}

// TestTick_SkipsChatBelowWindow verifies that chats with ≤ verbatimWindow
// turns are left untouched.
func TestTick_SkipsChatBelowWindow(t *testing.T) {
	const window = 5
	chatID := int64(1002)

	store := newFakeStore()
	store.addTurns(chatID, makeTurns(3)) // 3 turns < window=5

	sum := &fakeSummarizer{returnFn: func(string, []memory.Turn) (string, error) {
		return "should not be called", nil
	}}

	loop := NewLoop(store, sum, window, time.Hour, slog.Default())
	loop.tick(context.Background())

	if sum.callCount() != 0 {
		t.Errorf("Summarize should not have been called for chat below window, called %d times", sum.callCount())
	}

	remaining, _ := store.RecentTurns(context.Background(), chatID, 1000)
	if len(remaining) != 3 {
		t.Errorf("no turns should have been deleted, got %d remaining", len(remaining))
	}
}

// TestTick_ContinuesAfterSummarizerError verifies that a Summarizer failure
// for one chat does not prevent other chats from being processed.
// The test is written order-independently because ChatsNeedingSummary iterates
// a map whose traversal order is not guaranteed.
func TestTick_ContinuesAfterSummarizerError(t *testing.T) {
	const window = 2
	chatA := int64(1003)
	chatB := int64(1004)

	store := newFakeStore()
	store.addTurns(chatA, makeTurns(5))
	store.addTurns(chatB, makeTurns(5))

	var mu sync.Mutex
	callCount := 0

	// Fail the first chat that's processed (whichever order the store returns),
	// succeed for the second.
	sum := &fakeSummarizer{returnFn: func(existing string, turns []memory.Turn) (string, error) {
		mu.Lock()
		n := callCount
		callCount++
		mu.Unlock()
		if n == 0 {
			return "", errors.New("first chat fails")
		}
		return "ok", nil
	}}

	loop := NewLoop(store, sum, window, time.Hour, slog.Default())
	loop.tick(context.Background())

	if sum.callCount() != 2 {
		t.Fatalf("want 2 Summarize calls (one per chat), got %d", sum.callCount())
	}

	// Exactly one chat should have a summary (the second one processed succeeded).
	ctx := context.Background()
	summA, _ := store.GetSummary(ctx, chatA)
	summB, _ := store.GetSummary(ctx, chatB)

	summaryCount := 0
	if summA != nil {
		summaryCount++
	}
	if summB != nil {
		summaryCount++
	}
	if summaryCount != 1 {
		t.Errorf("want exactly 1 chat with a summary (one failed, one succeeded), got %d", summaryCount)
	}
}

// TestTick_FoldsExistingSummary verifies that an existing summary text is
// passed as the `existing` argument to Summarize on subsequent ticks.
func TestTick_FoldsExistingSummary(t *testing.T) {
	const window = 1
	chatID := int64(1005)

	store := newFakeStore()
	store.addTurns(chatID, makeTurns(3)) // 3 turns, window=1

	firstResult := "first summary"
	call := 0
	sum := &fakeSummarizer{returnFn: func(existing string, turns []memory.Turn) (string, error) {
		call++
		if call == 1 {
			return firstResult, nil
		}
		// Second call should receive first summary as existing.
		if existing != firstResult {
			return "", errors.New("expected existing to be first summary")
		}
		return "second summary", nil
	}}

	loop := NewLoop(store, sum, window, time.Hour, slog.Default())

	// First tick: summarizes 2 oldest turns, stores summary.
	loop.tick(context.Background())

	// Add more turns to trigger a second tick.
	store.addTurns(chatID, makeTurns(2))

	// Second tick: should pass the stored summary as existing.
	loop.tick(context.Background())

	if call != 2 {
		t.Fatalf("want 2 Summarize calls, got %d", call)
	}
}

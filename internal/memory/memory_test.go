package memory_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/memory"
)

// openTestPool opens a pool using TEST_DATABASE_URL and applies migrations.
// Tests call t.Skip if the env var is not set.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration tests")
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

// uniqueID returns a large int64 unlikely to collide with other test runs.
// Each call to t.Run gets its own chatID, preventing cross-test pollution.
func uniqueChatID() int64 {
	return time.Now().UnixNano()
}

// cleanupChat removes all rows for a chat ID across all three tables.
func cleanupChat(t *testing.T, pool *pgxpool.Pool, chatID int64) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		"DELETE FROM chat_turns    WHERE telegram_chat_id = $1",
		"DELETE FROM chat_summaries WHERE telegram_chat_id = $1",
		"DELETE FROM chat_facts    WHERE telegram_chat_id = $1",
	} {
		if _, err := pool.Exec(ctx, q, chatID); err != nil {
			t.Errorf("cleanup chat %d: %v", chatID, err)
		}
	}
}

// ── Turns ─────────────────────────────────────────────────────────────────────

func TestAppendAndRecentTurns(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()
	t.Cleanup(func() { cleanupChat(t, pool, chatID) })

	turns := []memory.Turn{
		{Role: memory.RoleUser, Content: "hello"},
		{Role: memory.RoleAssistant, Content: "hi there"},
		{Role: memory.RoleUser, Content: "remind me tomorrow"},
	}
	for _, turn := range turns {
		if err := store.AppendTurn(ctx, chatID, turn); err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
	}

	got, err := store.RecentTurns(ctx, chatID, 10)
	if err != nil {
		t.Fatalf("RecentTurns: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 turns, got %d", len(got))
	}
	// RecentTurns must return chronological order (oldest first).
	for i, want := range turns {
		if got[i].Role != want.Role || got[i].Content != want.Content {
			t.Errorf("turn[%d]: want {%s %q}, got {%s %q}", i, want.Role, want.Content, got[i].Role, got[i].Content)
		}
	}
}

func TestRecentTurns_Limit(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()
	t.Cleanup(func() { cleanupChat(t, pool, chatID) })

	for i := range 5 {
		_ = i
		if err := store.AppendTurn(ctx, chatID, memory.Turn{Role: memory.RoleUser, Content: "msg"}); err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
	}

	got, err := store.RecentTurns(ctx, chatID, 2)
	if err != nil {
		t.Fatalf("RecentTurns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 turns (limit enforced), got %d", len(got))
	}
}

func TestRecentTurns_Empty(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()

	got, err := store.RecentTurns(ctx, chatID, 10)
	if err != nil {
		t.Fatalf("RecentTurns on empty chat: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 turns, got %d", len(got))
	}
}

// ── Summary ───────────────────────────────────────────────────────────────────

func TestGetSummary_NoneExists(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()

	got, err := store.GetSummary(ctx, uniqueChatID())
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil summary, got %+v", got)
	}
}

func TestUpsertAndGetSummary(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()
	t.Cleanup(func() { cleanupChat(t, pool, chatID) })

	through := time.Now().UTC().Truncate(time.Millisecond)
	want := memory.Summary{Text: "so far we discussed reminders", SummarizedThrough: through}

	if err := store.UpsertSummary(ctx, chatID, want); err != nil {
		t.Fatalf("UpsertSummary: %v", err)
	}

	got, err := store.GetSummary(ctx, chatID)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if got == nil {
		t.Fatal("want summary, got nil")
	}
	if got.Text != want.Text {
		t.Errorf("Text: want %q, got %q", want.Text, got.Text)
	}
	if !got.SummarizedThrough.Equal(want.SummarizedThrough) {
		t.Errorf("SummarizedThrough: want %v, got %v", want.SummarizedThrough, got.SummarizedThrough)
	}

	// Second upsert should overwrite.
	want2 := memory.Summary{Text: "updated summary", SummarizedThrough: through.Add(time.Minute)}
	if err := store.UpsertSummary(ctx, chatID, want2); err != nil {
		t.Fatalf("UpsertSummary (second): %v", err)
	}
	got2, _ := store.GetSummary(ctx, chatID)
	if got2 == nil || got2.Text != want2.Text {
		t.Errorf("upsert did not overwrite: got %+v", got2)
	}
}

func TestDeleteTurnsBefore(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()
	t.Cleanup(func() { cleanupChat(t, pool, chatID) })

	for i := range 4 {
		_ = i
		if err := store.AppendTurn(ctx, chatID, memory.Turn{Role: memory.RoleUser, Content: "msg"}); err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
	}

	allTurns, _ := store.RecentTurns(ctx, chatID, 100)
	if len(allTurns) != 4 {
		t.Fatalf("setup: want 4 turns, got %d", len(allTurns))
	}

	// Delete the two oldest; keep the two newest.
	cutoff := allTurns[1].CreatedAt
	if err := store.DeleteTurnsBefore(ctx, chatID, cutoff); err != nil {
		t.Fatalf("DeleteTurnsBefore: %v", err)
	}

	remaining, _ := store.RecentTurns(ctx, chatID, 100)
	if len(remaining) != 2 {
		t.Fatalf("want 2 turns after delete, got %d", len(remaining))
	}
}

// ── Facts ─────────────────────────────────────────────────────────────────────

func TestAddAndFacts(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()
	t.Cleanup(func() { cleanupChat(t, pool, chatID) })

	f1 := memory.Fact{
		Kind:    memory.KindPreference,
		Content: json.RawMessage(`{"topic":"coffee","detail":"black, no sugar"}`),
		Source:  memory.SourceUser,
	}
	f2 := memory.Fact{
		Kind:    memory.KindGoal,
		Content: json.RawMessage(`{"description":"exercise","cadence":"daily"}`),
		Source:  memory.SourceInferred,
	}

	if err := store.AddFact(ctx, chatID, f1); err != nil {
		t.Fatalf("AddFact f1: %v", err)
	}
	if err := store.AddFact(ctx, chatID, f2); err != nil {
		t.Fatalf("AddFact f2: %v", err)
	}

	facts, err := store.Facts(ctx, chatID)
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("want 2 facts, got %d", len(facts))
	}
	if facts[0].Kind != memory.KindPreference || facts[1].Kind != memory.KindGoal {
		t.Errorf("unexpected kinds: %v %v", facts[0].Kind, facts[1].Kind)
	}
}

func TestFactsByKind(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()
	t.Cleanup(func() { cleanupChat(t, pool, chatID) })

	for range 3 {
		_ = store.AddFact(ctx, chatID, memory.Fact{
			Kind:    memory.KindPerson,
			Content: json.RawMessage(`{"name":"Alice","relation":"friend"}`),
			Source:  memory.SourceUser,
		})
	}
	_ = store.AddFact(ctx, chatID, memory.Fact{
		Kind:    memory.KindRoutine,
		Content: json.RawMessage(`{"description":"run","when":"morning"}`),
		Source:  memory.SourceUser,
	})

	persons, err := store.FactsByKind(ctx, chatID, memory.KindPerson)
	if err != nil {
		t.Fatalf("FactsByKind: %v", err)
	}
	if len(persons) != 3 {
		t.Fatalf("want 3 person facts, got %d", len(persons))
	}
}

func TestDeleteFact(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()
	t.Cleanup(func() { cleanupChat(t, pool, chatID) })

	id := uuid.New()
	if err := store.AddFact(ctx, chatID, memory.Fact{
		ID:      id,
		Kind:    memory.KindPreference,
		Content: json.RawMessage(`{"topic":"music","detail":"jazz"}`),
		Source:  memory.SourceUser,
	}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}

	if err := store.DeleteFact(ctx, chatID, id); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}

	facts, _ := store.Facts(ctx, chatID)
	if len(facts) != 0 {
		t.Fatalf("want 0 facts after delete, got %d", len(facts))
	}
}

// ── ForgetChat ────────────────────────────────────────────────────────────────

func TestForgetChat(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()
	chatID := uniqueChatID()
	// No cleanup needed — ForgetChat is the cleanup.

	// Seed turns, a summary, and a fact.
	_ = store.AppendTurn(ctx, chatID, memory.Turn{Role: memory.RoleUser, Content: "hello"})
	_ = store.UpsertSummary(ctx, chatID, memory.Summary{Text: "hi", SummarizedThrough: time.Now()})
	_ = store.AddFact(ctx, chatID, memory.Fact{
		Kind:    memory.KindPreference,
		Content: json.RawMessage(`{"topic":"t","detail":"d"}`),
		Source:  memory.SourceUser,
	})

	if err := store.ForgetChat(ctx, chatID); err != nil {
		t.Fatalf("ForgetChat: %v", err)
	}

	turns, _ := store.RecentTurns(ctx, chatID, 100)
	if len(turns) != 0 {
		t.Errorf("want 0 turns after ForgetChat, got %d", len(turns))
	}
	sum, _ := store.GetSummary(ctx, chatID)
	if sum != nil {
		t.Errorf("want nil summary after ForgetChat, got %+v", sum)
	}
	facts, _ := store.Facts(ctx, chatID)
	if len(facts) != 0 {
		t.Errorf("want 0 facts after ForgetChat, got %d", len(facts))
	}
}

// ── ChatsNeedingSummary ───────────────────────────────────────────────────────

func TestChatsNeedingSummary(t *testing.T) {
	pool := openTestPool(t)
	store := memory.NewPostgresStore(pool)
	ctx := context.Background()

	chatA := uniqueChatID()
	chatB := uniqueChatID()
	chatC := uniqueChatID()
	t.Cleanup(func() {
		cleanupChat(t, pool, chatA)
		cleanupChat(t, pool, chatB)
		cleanupChat(t, pool, chatC)
	})

	// chatA: 3 turns, chatB: 1 turn, chatC: 5 turns. Window = 2.
	appendN := func(chatID int64, n int) {
		for range n {
			_ = store.AppendTurn(ctx, chatID, memory.Turn{Role: memory.RoleUser, Content: "x"})
		}
	}
	appendN(chatA, 3)
	appendN(chatB, 1)
	appendN(chatC, 5)

	chats, err := store.ChatsNeedingSummary(ctx, 2)
	if err != nil {
		t.Fatalf("ChatsNeedingSummary: %v", err)
	}

	needs := map[int64]bool{}
	for _, id := range chats {
		needs[id] = true
	}
	if !needs[chatA] {
		t.Errorf("chatA (3 turns) should need summary with window=2")
	}
	if needs[chatB] {
		t.Errorf("chatB (1 turn) should NOT need summary with window=2")
	}
	if !needs[chatC] {
		t.Errorf("chatC (5 turns) should need summary with window=2")
	}
}

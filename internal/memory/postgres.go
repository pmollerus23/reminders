package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── ForgetChat ───────────────────────────────────────────────────────────────

type postgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a Store backed by Postgres.
func NewPostgresStore(pool *pgxpool.Pool) Store {
	return &postgresStore{pool: pool}
}

// ── Turns ────────────────────────────────────────────────────────────────────

const appendTurnQuery = `
	INSERT INTO chat_turns (telegram_chat_id, role, content)
	VALUES ($1, $2, $3)
`

func (s *postgresStore) AppendTurn(ctx context.Context, chatID int64, t Turn) error {
	_, err := s.pool.Exec(ctx, appendTurnQuery, chatID, t.Role, t.Content)
	if err != nil {
		return fmt.Errorf("memory: append turn: %w", err)
	}
	return nil
}

// recentTurnsQuery returns newest-first; we reverse in Go so callers always
// see chronological order.
const recentTurnsQuery = `
	SELECT role, content, created_at
	FROM chat_turns
	WHERE telegram_chat_id = $1
	ORDER BY created_at DESC
	LIMIT $2
`

func (s *postgresStore) RecentTurns(ctx context.Context, chatID int64, limit int) ([]Turn, error) {
	rows, err := s.pool.Query(ctx, recentTurnsQuery, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("memory: query recent turns: %w", err)
	}
	defer rows.Close()

	var turns []Turn
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.Role, &t.Content, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("memory: scan turn: %w", err)
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate turns: %w", err)
	}

	// Query is DESC (cheapest index direction); reverse to chronological order.
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns, nil
}

// ── Summary ──────────────────────────────────────────────────────────────────

const getSummaryQuery = `
	SELECT summary, summarized_through
	FROM chat_summaries
	WHERE telegram_chat_id = $1
`

// GetSummary returns (nil, nil) when no summary exists — callers treat absence
// as "fresh chat", not as an error.
func (s *postgresStore) GetSummary(ctx context.Context, chatID int64) (*Summary, error) {
	var sum Summary
	err := s.pool.QueryRow(ctx, getSummaryQuery, chatID).Scan(
		&sum.Text,
		&sum.SummarizedThrough,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: get summary: %w", err)
	}
	return &sum, nil
}

const upsertSummaryQuery = `
	INSERT INTO chat_summaries (telegram_chat_id, summary, summarized_through)
	VALUES ($1, $2, $3)
	ON CONFLICT (telegram_chat_id) DO UPDATE
		SET summary            = EXCLUDED.summary,
		    summarized_through = EXCLUDED.summarized_through,
		    updated_at         = now()
`

func (s *postgresStore) UpsertSummary(ctx context.Context, chatID int64, sum Summary) error {
	_, err := s.pool.Exec(ctx, upsertSummaryQuery, chatID, sum.Text, sum.SummarizedThrough)
	if err != nil {
		return fmt.Errorf("memory: upsert summary: %w", err)
	}
	return nil
}

const deleteTurnsBeforeQuery = `
	DELETE FROM chat_turns
	WHERE telegram_chat_id = $1
	  AND created_at <= $2
`

func (s *postgresStore) DeleteTurnsBefore(ctx context.Context, chatID int64, cutoff time.Time) error {
	_, err := s.pool.Exec(ctx, deleteTurnsBeforeQuery, chatID, cutoff)
	if err != nil {
		return fmt.Errorf("memory: delete turns before: %w", err)
	}
	return nil
}

// ── Facts ────────────────────────────────────────────────────────────────────

const addFactQuery = `
	INSERT INTO chat_facts (id, telegram_chat_id, kind, content, source)
	VALUES ($1, $2, $3, $4, $5)
`

func (s *postgresStore) AddFact(ctx context.Context, chatID int64, f Fact) error {
	id := f.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := s.pool.Exec(ctx, addFactQuery, id, chatID, f.Kind, f.Content, f.Source)
	if err != nil {
		return fmt.Errorf("memory: add fact: %w", err)
	}
	return nil
}

const factsQuery = `
	SELECT id, kind, content, source, created_at, updated_at
	FROM chat_facts
	WHERE telegram_chat_id = $1
	ORDER BY created_at ASC
`

func (s *postgresStore) Facts(ctx context.Context, chatID int64) ([]Fact, error) {
	rows, err := s.pool.Query(ctx, factsQuery, chatID)
	if err != nil {
		return nil, fmt.Errorf("memory: query facts: %w", err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(
			&f.ID,
			&f.Kind,
			&f.Content,
			&f.Source,
			&f.CreatedAt,
			&f.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("memory: scan fact: %w", err)
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate facts: %w", err)
	}
	return facts, nil
}

const factsByKindQuery = `
	SELECT id, kind, content, source, created_at, updated_at
	FROM chat_facts
	WHERE telegram_chat_id = $1
	  AND kind = $2
	ORDER BY created_at ASC
`

func (s *postgresStore) FactsByKind(ctx context.Context, chatID int64, kind Kind) ([]Fact, error) {
	rows, err := s.pool.Query(ctx, factsByKindQuery, chatID, kind)
	if err != nil {
		return nil, fmt.Errorf("memory: query facts by kind: %w", err)
	}
	defer rows.Close()

	var facts []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(
			&f.ID,
			&f.Kind,
			&f.Content,
			&f.Source,
			&f.CreatedAt,
			&f.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("memory: scan fact: %w", err)
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate facts: %w", err)
	}
	return facts, nil
}

const deleteFactQuery = `
	DELETE FROM chat_facts
	WHERE telegram_chat_id = $1
	  AND id = $2
`

func (s *postgresStore) DeleteFact(ctx context.Context, chatID int64, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, deleteFactQuery, chatID, id)
	if err != nil {
		return fmt.Errorf("memory: delete fact: %w", err)
	}
	return nil
}

const updateFactQuery = `
	UPDATE chat_facts
	SET kind       = $1,
	    content    = $2,
	    updated_at = now()
	WHERE telegram_chat_id = $3
	  AND id = $4
`

func (s *postgresStore) UpdateFact(ctx context.Context, chatID int64, id uuid.UUID, kind Kind, content json.RawMessage) error {
	tag, err := s.pool.Exec(ctx, updateFactQuery, kind, content, chatID, id)
	if err != nil {
		return fmt.Errorf("memory: update fact: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("memory: fact not found: %s", id)
	}
	return nil
}

// ── Summarize support ────────────────────────────────────────────────────────

// chatsNeedingSummaryQuery finds chats with more turns than verbatimWindow.
// The summarize loop uses this to discover work each tick.
const chatsNeedingSummaryQuery = `
	SELECT telegram_chat_id
	FROM chat_turns
	GROUP BY telegram_chat_id
	HAVING COUNT(*) > $1
`

func (s *postgresStore) ChatsNeedingSummary(ctx context.Context, verbatimWindow int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, chatsNeedingSummaryQuery, verbatimWindow)
	if err != nil {
		return nil, fmt.Errorf("memory: query chats needing summary: %w", err)
	}
	defer rows.Close()

	var chatIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("memory: scan chat id: %w", err)
		}
		chatIDs = append(chatIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate chat ids: %w", err)
	}
	return chatIDs, nil
}

// ── ForgetChat ───────────────────────────────────────────────────────────────

// ForgetChat removes all turns, the summary, and all facts for chatID in a
// single transaction. The three deletes are ordered so the most numerous
// table (turns) goes first, but all three are unconditional — missing rows
// are silently ignored by DELETE.
func (s *postgresStore) ForgetChat(ctx context.Context, chatID int64) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("memory: forget chat: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, q := range []string{
		"DELETE FROM chat_turns    WHERE telegram_chat_id = $1",
		"DELETE FROM chat_summaries WHERE telegram_chat_id = $1",
		"DELETE FROM chat_facts    WHERE telegram_chat_id = $1",
	} {
		if _, err := tx.Exec(ctx, q, chatID); err != nil {
			return fmt.Errorf("memory: forget chat: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("memory: forget chat: commit: %w", err)
	}
	return nil
}

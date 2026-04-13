package memory

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Kind is the semantic category of a stored fact. The vocabulary is defined
// here rather than as a DB CHECK constraint so that adding a new Kind only
// requires a Go change, not a migration.
type Kind string

const (
	KindPreference Kind = "preference"
	KindGoal       Kind = "goal"
	KindPerson     Kind = "person"
	KindRoutine    Kind = "routine"
)

type Source string

const (
	SourceUser     Source = "user"
	SourceInferred Source = "inferred"
)

// Turn is one message in a conversation — either from the human or the bot.
type Turn struct {
	Role      Role
	Content   string
	CreatedAt time.Time
}

// Fact is a long-lived piece of information about a user. Content is kept as
// raw JSON because the shape varies by Kind; callers decode it themselves.
type Fact struct {
	ID        uuid.UUID
	Kind      Kind
	Content   json.RawMessage // decoded by caller based on Kind
	Source    Source
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Summary is the rolling compressed history for a chat.
type Summary struct {
	Text              string
	SummarizedThrough time.Time // newest Turn.CreatedAt folded into this summary
}

// Store is the persistence interface for the memory layer.
// It is defined here (consumer side) so callers don't import the postgres package.
type Store interface {
	// Turns
	AppendTurn(ctx context.Context, chatID int64, t Turn) error
	// RecentTurns returns the newest `limit` turns in chronological order
	// (oldest first). Returns an empty slice, not an error, when none exist.
	RecentTurns(ctx context.Context, chatID int64, limit int) ([]Turn, error)

	// Summary
	// GetSummary returns nil, nil when no summary exists for the chat.
	GetSummary(ctx context.Context, chatID int64) (*Summary, error)
	UpsertSummary(ctx context.Context, chatID int64, s Summary) error
	DeleteTurnsBefore(ctx context.Context, chatID int64, cutoff time.Time) error

	// Facts
	AddFact(ctx context.Context, chatID int64, f Fact) error
	Facts(ctx context.Context, chatID int64) ([]Fact, error)
	FactsByKind(ctx context.Context, chatID int64, kind Kind) ([]Fact, error)
	DeleteFact(ctx context.Context, chatID int64, id uuid.UUID) error

	// ChatsNeedingSummary returns chat IDs with more than verbatimWindow turns.
	// Used by the summarize loop to find work to do.
	ChatsNeedingSummary(ctx context.Context, verbatimWindow int) ([]int64, error)

	// ForgetChat removes all turns, the summary, and all facts for chatID in a
	// single transaction. Useful for testing and eventual "forget me" commands.
	ForgetChat(ctx context.Context, chatID int64) error
}

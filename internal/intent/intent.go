// Package intent provides the unified natural-language dispatcher. One agentic
// loop determines the user's intent, executes any necessary tool calls (via
// caller-supplied callbacks), and returns the model's final text reply — no
// separate routing or classification step required.
package intent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
)

// ReminderSummary is the view of a reminder returned to the model by
// list_reminders. IDs are strings so Claude can reference them in subsequent
// update_reminder / delete_reminder calls.
type ReminderSummary struct {
	ID         string `json:"id"`
	What       string `json:"what"`
	When       string `json:"when"` // RFC3339
	Recurrence string `json:"recurrence,omitempty"`
}

// FactSummary is the view of a fact returned to the model by list_facts.
type FactSummary struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Content json.RawMessage `json:"content"`
}

// ToolSet provides the callback functions the agentic loop invokes when the
// model calls any tool other than chat_response. Each function performs its
// side effect and returns a short confirmation string that is fed back to the
// model as the tool_result content, allowing the model to incorporate the
// outcome into its final reply.
type ToolSet struct {
	// ── Reminder write ───────────────────────────────────────────────────────

	// SetReminder persists a new reminder and returns a human-readable
	// confirmation, e.g. "Reminder set for 2026-04-14 09:00 EDT, then daily".
	SetReminder func(ctx context.Context, what string, when time.Time, recurrence *string) (string, error)

	// UpdateReminder modifies the body, time, and recurrence of an existing
	// pending reminder identified by its UUID string.
	UpdateReminder func(ctx context.Context, id string, what string, when time.Time, recurrence *string) (string, error)

	// DeleteReminder removes a reminder by UUID string.
	DeleteReminder func(ctx context.Context, id string) (string, error)

	// ── Reminder read ────────────────────────────────────────────────────────

	// ListReminders returns all pending reminders for the current chat.
	ListReminders func(ctx context.Context) ([]ReminderSummary, error)

	// ── Fact write ───────────────────────────────────────────────────────────

	// RememberFact persists a new structured fact and returns a confirmation.
	RememberFact func(ctx context.Context, kind memory.Kind, content json.RawMessage) (string, error)

	// UpdateFact replaces the kind and content of an existing fact.
	UpdateFact func(ctx context.Context, id string, kind memory.Kind, content json.RawMessage) (string, error)

	// DeleteFact removes a fact by UUID string.
	DeleteFact func(ctx context.Context, id string) (string, error)

	// ── Fact read ────────────────────────────────────────────────────────────

	// ListFacts returns all stored facts for the current chat.
	ListFacts func(ctx context.Context) ([]FactSummary, error)
}

// ErrDispatchRejected is the sentinel wrapped by DispatchError.
var ErrDispatchRejected = errors.New("dispatch rejected")

// DispatchError is a user-facing error from Dispatch. UserMessage is safe to
// send directly to the Telegram user. Infrastructure failures use plain errors,
// not DispatchError.
type DispatchError struct {
	UserMessage string
}

func (e *DispatchError) Error() string { return e.UserMessage }
func (e *DispatchError) Unwrap() error { return ErrDispatchRejected }

// Dispatcher runs an agentic loop against the LLM. It may call tools (via
// ToolSet) zero or more times before the model produces its final text reply.
// The returned string is the reply to send to the user.
//
// Infrastructure errors (API unavailable, DB failure inside a ToolSet callback)
// are returned as plain errors. User-facing rejections (e.g. stub mode) are
// returned as *DispatchError.
type Dispatcher interface {
	Dispatch(ctx context.Context, p promptctx.Prompt, now time.Time, loc *time.Location, tools ToolSet) (string, error)
}

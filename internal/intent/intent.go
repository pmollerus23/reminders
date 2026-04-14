// Package intent provides the unified natural-language dispatcher. One API
// call determines the user's intent (reminder, fact, or chat) and produces the
// full result — no separate routing or classification step required.
package intent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
)

// ActionKind names the three outcomes the dispatcher can produce.
type ActionKind string

const (
	ActionSetReminder  ActionKind = "set_reminder"
	ActionRememberFact ActionKind = "remember_fact"
	ActionChat         ActionKind = "chat"
)

// Action is the result of one Dispatch call. Exactly one group of payload
// fields is populated, determined by Kind.
type Action struct {
	Kind ActionKind

	// ActionSetReminder
	ReminderWhen       string  // RFC3339
	ReminderWhat       string
	ReminderRecurrence *string // nil = one-time; non-nil values: "hourly","daily","weekly","monthly"

	// ActionRememberFact
	FactKind    memory.Kind
	FactContent json.RawMessage

	// ActionChat
	ChatReply string
}

// ErrDispatchRejected is the sentinel wrapped by DispatchError.
var ErrDispatchRejected = errors.New("dispatch rejected")

// DispatchError is a user-facing error from Dispatch. UserMessage is safe to
// send directly to the Telegram user. It follows the same pattern as
// reminder.ParseError and fact.ParseError. Infrastructure failures use plain
// errors, not DispatchError.
type DispatchError struct {
	UserMessage string
}

func (e *DispatchError) Error() string { return e.UserMessage }
func (e *DispatchError) Unwrap() error  { return ErrDispatchRejected }

// Dispatcher takes a fully-assembled prompt (containing user context and the
// current message) and returns the Action the model chose to perform.
//
// Defined here alongside the Action type, following the project pattern of
// keeping interfaces close to the types they describe.
type Dispatcher interface {
	Dispatch(ctx context.Context, p promptctx.Prompt, now time.Time, loc *time.Location) (Action, error)
}

package reminder

import (
	"context"
	"errors"
	"time"
)

// ParsedReminder is the structured result of parsing a user's command.
type ParsedReminder struct {
	When time.Time
	What string
}

// ErrBadFormat signals the user's input couldn't be understood as a reminder.
// The handler replies with usage; the system doesn't treat this as an error.
var ErrBadFormat = errors.New("bad command format")

// ParseRequest is the input to any Parser implementation.
type ParseRequest struct {
	// Text is the reminder body with any command prefix already stripped.
	Text string
	// Now is the caller's current time, expressed in Loc. Injected rather
	// than read from time.Now() so parsers are pure functions of input.
	Now time.Time
	// Loc is the user's timezone. Parsers interpret relative expressions
	// against this location; returned When should carry this zone.
	Loc *time.Location
}

// Parser turns a reminder body into a structured ParsedReminder.
// Implementations must be safe for concurrent use.
type Parser interface {
	Parse(ctx context.Context, req ParseRequest) (ParsedReminder, error)
}

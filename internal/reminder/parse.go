package reminder

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ParsedReminder is the structured result of parsing a user's command.
type ParsedReminder struct {
	When time.Time
	What string
}

// ErrBadFormat is the semantic sentinel for "user input couldn't be parsed."
// Callers rarely match it directly now — they use errors.As(err, &ParseError)
// to get a user-facing message. It remains in the unwrap chain for any code
// that wants to ask the broader question.
var ErrBadFormat = errors.New("bad command format")

// ParseError is returned by Parser implementations when the user's input
// can't be understood. UserMessage is a human-readable explanation safe
// to show back to the user; the error chain (via Unwrap) carries
// ErrBadFormat so errors.Is(err, ErrBadFormat) still succeeds.
type ParseError struct {
	UserMessage string
	cause       error
}

func (e *ParseError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("parse: %s: %v", e.UserMessage, e.cause)
	}
	return fmt.Sprintf("parse: %s", e.UserMessage)
}

// Unwrap returns ErrBadFormat so errors.Is works at the semantic level.
// The private `cause` field is surfaced in Error() for logs but deliberately
// not in the unwrap chain — we don't want callers matching on incidental
// inner errors like *time.ParseError.
func (e *ParseError) Unwrap() error {
	return ErrBadFormat
}

func newParseError(userMsg string, cause error) *ParseError {
	return &ParseError{UserMessage: userMsg, cause: cause}
}

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

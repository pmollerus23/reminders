// Package chat provides the Responder interface and implementations for the
// /chat command. The Responder takes a fully-assembled promptctx.Prompt and
// returns the model's reply as a plain string.
package chat

import (
	"context"
	"errors"

	"github.com/pmollerus23/reminders/internal/promptctx"
)

// Responder generates a plain-text reply for one chat turn given an assembled prompt.
type Responder interface {
	Respond(ctx context.Context, p promptctx.Prompt) (string, error)
}

// ErrUnavailable is the sentinel wrapped by RespondError when the responder is
// configured but not functional (e.g. PARSER=regex).
var ErrUnavailable = errors.New("chat unavailable")

// RespondError is a user-facing error from Respond. UserMessage is safe to
// send directly to the Telegram user. It follows the same pattern as
// reminder.ParseError and fact.ParseError.
type RespondError struct {
	UserMessage string
	cause       error
}

func (e *RespondError) Error() string {
	return e.UserMessage
}

func (e *RespondError) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return ErrUnavailable
}

type stubResponder struct{}

// NewStubResponder returns a Responder that always returns a RespondError with
// a clear message. Used when PARSER=regex so that /chat gives a useful error
// rather than failing silently.
func NewStubResponder() Responder { return &stubResponder{} }

func (s *stubResponder) Respond(_ context.Context, _ promptctx.Prompt) (string, error) {
	return "", &RespondError{UserMessage: "The /chat command requires PARSER=claude."}
}

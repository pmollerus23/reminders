// Package fact handles the /remember command: parsing natural-language
// statements into structured memory.Fact values.
package fact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pmollerus23/reminders/internal/memory"
)

// ParsedFact is the structured output of a successful Parse call.
type ParsedFact struct {
	Kind    memory.Kind
	Content json.RawMessage // shape is determined by Kind; validated before return
}

// ParseRequest is the input to any Parser implementation.
type ParseRequest struct {
	// Text is the /remember body with the command prefix already stripped.
	Text string
}

// ErrBadFormat is the sentinel for "user input couldn't be understood as a fact."
// Callers use errors.As(err, &ParseError) to retrieve the user-facing message.
var ErrBadFormat = errors.New("bad fact format")

// ParseError is returned when input can't be parsed into a fact.
// Mirrors reminder.ParseError exactly — same pattern, same Unwrap sentinel trick.
type ParseError struct {
	UserMessage string
	cause       error
}

func (e *ParseError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("fact: %s: %v", e.UserMessage, e.cause)
	}
	return fmt.Sprintf("fact: %s", e.UserMessage)
}

// Unwrap returns ErrBadFormat so errors.Is(err, ErrBadFormat) works at the
// semantic level. The private cause is in Error() for logs only.
func (e *ParseError) Unwrap() error {
	return ErrBadFormat
}

func newParseError(userMsg string, cause error) *ParseError {
	return &ParseError{UserMessage: userMsg, cause: cause}
}

// Parser turns a /remember body into a structured ParsedFact.
// Implementations must be safe for concurrent use.
type Parser interface {
	Parse(ctx context.Context, req ParseRequest) (ParsedFact, error)
}

// ── Stub parser ───────────────────────────────────────────────────────────────

type stubParser struct{}

// NewStubParser returns a Parser that always rejects with a clear message.
// Used when PARSER=regex, since /remember only makes sense with LLM parsing.
// Both parsers share the PARSER knob to avoid a redundant env var — the stub
// makes the coupling explicit rather than silently doing nothing.
func NewStubParser() Parser {
	return &stubParser{}
}

func (p *stubParser) Parse(_ context.Context, _ ParseRequest) (ParsedFact, error) {
	return ParsedFact{}, newParseError("The /remember command requires PARSER=claude", nil)
}

package scheduler

import "context"

// ReminderAgent turns a raw reminder body into a natural, conversational
// message for delivery. It is defined here, in the consumer package.
type ReminderAgent interface {
	Compose(ctx context.Context, body string) (string, error)
}

// passthroughAgent returns the raw body unchanged. Used when the Claude API
// is not available (PARSER=regex).
type passthroughAgent struct{}

func (passthroughAgent) Compose(_ context.Context, body string) (string, error) {
	return body, nil
}

// NewPassthroughAgent returns a ReminderAgent that sends reminders verbatim.
func NewPassthroughAgent() ReminderAgent { return passthroughAgent{} }

package scheduler

import "context"

// Messenger is the outbound side of a messaging platform.
// It is defined here, in the consumer package, so the scheduler
// declares only what it needs. Implementations live elsewhere
// (e.g. internal/telegram) and are injected at startup.
type Messenger interface {
	Send(ctx context.Context, chatID int64, body string) error
}

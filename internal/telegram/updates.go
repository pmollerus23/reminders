package telegram

import (
	"context"
)

// Update is our own minimal projection of a Telegram update. We deliberately
// do not expose the library's *models.Update — keeping the dependency
// contained here means consumers (handler, tests) don't transitively
// depend on go-telegram/bot, and swapping libraries stays local.
type Update struct {
	UpdateID  int64
	ChatID    int64
	MessageID int64
	Text      string
}

// Handler is satisfied by anything that can process one update.
// Defined here because the dispatcher (Client) is the consumer.
// The concrete implementation lives at the composition root.
type Handler interface {
	Handle(ctx context.Context, u Update) error
}

package telegram

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Client struct {
	bot    *bot.Bot
	logger *slog.Logger

	// handler is set exactly once via AttachHandler, before Start is called.
	// No mutex: the Go memory model guarantees that writes happening-before
	// a `go` statement are visible to the started goroutine. Since main.go
	// calls AttachHandler before the errgroup goroutine invokes Start, the
	// library's dispatch goroutines observe the write without synchronization.
	// Do NOT mutate handler after Start.
	handler Handler
}

// New constructs a Client. All inbound text messages (commands and plain text
// alike) are routed to the registered Handler via WithDefaultHandler. The
// handler.Handle method is responsible for ignoring unknown commands and empty
// messages — no routing is done at the Telegram layer.
func New(ctx context.Context, token string, logger *slog.Logger) (*Client, error) {
	c := &Client{logger: logger}

	opts := []bot.Option{
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message"}),
		// Route every message — commands and plain text — to c.dispatch.
		// The Handler decides what to act on; the library only translates the
		// update shape and logs errors.
		bot.WithDefaultHandler(c.dispatch),
		bot.WithErrorsHandler(func(err error) {
			c.logger.Error("telegram library error", "err", err)
		}),
	}

	b, err := bot.New(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("telegram: new bot: %w", err)
	}

	c.bot = b
	return c, nil
}

// AttachHandler sets the Handler that messages will be dispatched to.
// Must be called before Start. Calling it twice is a programming error
// (the second call overwrites the first with no synchronization guarantees
// once Start is running).
func (c *Client) AttachHandler(h Handler) {
	c.handler = h
}

// Start runs the library's long-poll loop. Blocks until ctx is cancelled.
func (c *Client) Start(ctx context.Context) error {
	c.logger.Info("telegram updater starting")
	c.bot.Start(ctx)
	c.logger.Info("telegram updater stopped")
	return nil
}

// Send delivers a plain-text message to the given chat.
func (c *Client) Send(ctx context.Context, chatID int64, body string) error {
	_, err := c.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   body,
	})
	if err != nil {
		return fmt.Errorf("telegram: send message: %w", err)
	}
	return nil
}

// dispatch is invoked by the library for every inbound message update.
// It translates the library's update shape into ours and delegates to the
// registered Handler. The Handler is responsible for ignoring irrelevant
// messages — this method does no routing.
func (c *Client) dispatch(ctx context.Context, _ *bot.Bot, u *models.Update) {
	if u == nil || u.Message == nil {
		return
	}

	if c.handler == nil {
		c.logger.Warn("message received but no handler attached", "update_id", u.ID)
		return
	}

	our := Update{
		UpdateID:  u.ID,
		ChatID:    u.Message.Chat.ID,
		MessageID: int64(u.Message.ID),
		Text:      u.Message.Text,
	}

	if err := c.handler.Handle(ctx, our); err != nil {
		c.logger.Error("handle update failed",
			"update_id", our.UpdateID,
			"chat_id", our.ChatID,
			"err", err,
		)
	}
}

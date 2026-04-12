package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Client struct {
	bot    *bot.Bot
	logger *slog.Logger

	// handler is set after construction via AttachHandler, before Start.
	// Guarded by mu because the library's dispatch goroutine reads it
	// concurrently with AttachHandler — though in practice AttachHandler
	// is always called before Start, so contention is zero. The mutex
	// is belt-and-suspenders for correctness, not performance.
	mu      sync.RWMutex
	handler Handler
}

// New constructs a Client and registers an internal default handler
// that dispatches to whatever Handler is attached via AttachHandler.
func New(ctx context.Context, token string, logger *slog.Logger) (*Client, error) {
	c := &Client{logger: logger}

	opts := []bot.Option{
		bot.WithDefaultHandler(c.dispatch),
	}

	b, err := bot.New(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("telegram: new bot: %w", err)
	}
	c.bot = b
	return c, nil
}

// AttachHandler sets the handler that will receive inbound updates.
// Must be called before Start. Calling it twice replaces the handler.
func (c *Client) AttachHandler(h Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = h
}

// Start runs the library's long-poll loop. Blocks until ctx is cancelled.
// Returns nil on clean shutdown (bot.Start has no return value — it logs
// and retries on transient errors, exits on ctx cancellation).
func (c *Client) Start(ctx context.Context) error {
	c.logger.Info("telegram updater starting")
	c.bot.Start(ctx)
	c.logger.Info("telegram updater stopped")
	return nil
}

// Send — unchanged.
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

// dispatch is the library's HandlerFunc signature. It translates the
// library's update shape into ours and delegates to the attached Handler.
func (c *Client) dispatch(ctx context.Context, _ *bot.Bot, u *models.Update) {
	// Filter: we only care about text messages. Edited messages, callbacks,
	// polls, etc. get dropped silently for now. Expand as needs arise.
	if u == nil || u.Message == nil || u.Message.Text == "" {
		return
	}

	c.mu.RLock()
	h := c.handler
	c.mu.RUnlock()

	if h == nil {
		c.logger.Warn("update received but no handler attached", "update_id", u.ID)
		return
	}

	our := Update{
		UpdateID:  u.ID,
		ChatID:    u.Message.Chat.ID,
		MessageID: int64(u.Message.ID),
		Text:      u.Message.Text,
	}

	if err := h.Handle(ctx, our); err != nil {
		// Handler errors aren't fatal — the library swallows panics and
		// we log-and-move-on for errors. The alternative (killing the
		// daemon on one bad message) is worse behavior for a bot.
		c.logger.Error("handle update failed",
			"update_id", our.UpdateID,
			"chat_id", our.ChatID,
			"err", err,
		)
	}
}

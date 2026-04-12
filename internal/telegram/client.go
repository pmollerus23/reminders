package telegram

import (
	"context"
	"fmt"

	"github.com/go-telegram/bot"
)

// Client wraps a Telegram Bot API client. It satisfies the
// scheduler.Messenger interface implicitly — Go has no "implements"
// keyword. Whether this type satisfies an interface is checked at
// the call site where it's used as that interface type.
type Client struct {
	bot *bot.Bot
}

// New constructs a Client. It calls getMe under the hood to verify
// the token is valid, so a returned error here means "your token is
// wrong" and should fail startup loudly.
func New(ctx context.Context, token string) (*Client, error) {
	b, err := bot.New(token)
	if err != nil {
		return nil, fmt.Errorf("telegram: new bot: %w", err)
	}
	return &Client{bot: b}, nil
}

// Send delivers a plain-text message to the given chat.
// The signature matches scheduler.Messenger exactly.
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

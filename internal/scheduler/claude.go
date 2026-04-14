package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const composeSystemPrompt = `You are a friendly personal assistant delivering a reminder on behalf of the user.

Write a brief, warm, natural-sounding message to notify them. A few guidelines:
- Be conversational — avoid stiff or robotic phrasing
- Keep it short (1–2 sentences is ideal)
- Do NOT say who you are or apologise for interrupting
- Return only the message text, nothing else`

type claudeReminderAgent struct {
	client anthropic.Client
	logger *slog.Logger
}

// NewClaudeReminderAgent returns a ReminderAgent that calls the Anthropic API
// to compose a natural delivery message for each reminder.
func NewClaudeReminderAgent(apiKey string, logger *slog.Logger) ReminderAgent {
	return &claudeReminderAgent{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
		logger: logger,
	}
}

// Compose calls Claude to rewrite the raw reminder body as a conversational message.
// On any API error it falls back to the raw body so reminders are never silently dropped.
func (a *claudeReminderAgent) Compose(ctx context.Context, body string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 256,
		System: []anthropic.TextBlockParam{
			{Text: composeSystemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(
				anthropic.NewTextBlock(fmt.Sprintf("Reminder: %s", body)),
			),
		},
	})
	if err != nil {
		a.logger.Warn("reminder agent: api call failed, falling back to raw body",
			"body", body, "err", err)
		return body, nil
	}

	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			if msg := strings.TrimSpace(tb.Text); msg != "" {
				a.logger.Debug("reminder agent: composed message", "original", body, "composed", msg)
				return msg, nil
			}
		}
	}

	a.logger.Warn("reminder agent: no text in response, falling back to raw body", "body", body)
	return body, nil
}

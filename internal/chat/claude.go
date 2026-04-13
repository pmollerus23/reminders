package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
)

type claudeResponder struct {
	client anthropic.Client
	logger *slog.Logger
}

// NewClaudeResponder returns a Responder backed by the Anthropic API.
// The client is constructed once and reused; it is safe for concurrent use.
func NewClaudeResponder(apiKey string, logger *slog.Logger) Responder {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &claudeResponder{client: client, logger: logger}
}

func (r *claudeResponder) Respond(ctx context.Context, p promptctx.Prompt) (string, error) {
	// Longer timeout than parsers — open-ended chat replies can be verbose.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	messages := make([]anthropic.MessageParam, len(p.Messages))
	for i, m := range p.Messages {
		messages[i] = toMessageParam(m)
	}

	resp, err := r.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: p.System},
		},
		Messages: messages,
	})
	if err != nil {
		return "", fmt.Errorf("chat: api call: %w", err)
	}

	var text string
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text = tb.Text
			break
		}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("chat: empty response from API")
	}

	return text, nil
}

// toMessageParam converts a promptctx.Message to the SDK type.
// Assistant turns use NewAssistantMessage; everything else (user, or any
// unexpected role) uses NewUserMessage so the conversation is never malformed.
func toMessageParam(m promptctx.Message) anthropic.MessageParam {
	if m.Role == memory.RoleAssistant {
		return anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content))
	}
	return anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content))
}

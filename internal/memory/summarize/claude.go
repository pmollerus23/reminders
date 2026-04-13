package summarize

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/pmollerus23/reminders/internal/memory"
)

type claudeSummarizer struct {
	client anthropic.Client
	logger *slog.Logger
}

// NewClaudeSummarizer returns a Summarizer backed by the Anthropic API.
// The client is constructed once and reused; it is safe for concurrent use.
func NewClaudeSummarizer(apiKey string, logger *slog.Logger) Summarizer {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &claudeSummarizer{client: client, logger: logger}
}

const summarizeSystemPrompt = `You are maintaining a rolling summary of a user's conversations with a
reminder bot. Your job is to produce a concise, durable summary that
captures stable facts about the user and ongoing themes — not one-off
details like specific reminder times or dates.

You will receive:
1. The existing summary (may be empty for new chats).
2. New conversational turns since that summary was written.

Produce an updated summary that integrates the new turns. Stay under
2000 characters. Focus on recurring topics, stated preferences,
long-term goals, and relationships. Omit transient details. Write in
third person ("The user..."). If there is nothing substantive to
summarize, return the existing summary unchanged.`

func (s *claudeSummarizer) Summarize(ctx context.Context, existing string, newTurns []memory.Turn) (string, error) {
	// Bound the call — summarize loop's ctx may have no deadline.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	userMsg := buildSummarizeMessage(existing, newTurns)

	resp, err := s.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: summarizeSystemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userMsg)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("claude: summarize api call: %w", err)
	}

	var text string
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text = tb.Text
			break
		}
	}

	text = strings.TrimSpace(text)
	if err := validateSummary(text); err != nil {
		s.logger.Warn("summarize: output validation failed", "err", err)
		return "", fmt.Errorf("claude: %w", err)
	}

	return text, nil
}

// buildSummarizeMessage formats existing summary + new turns into a single
// user message block for the LLM.
func buildSummarizeMessage(existing string, turns []memory.Turn) string {
	var sb strings.Builder
	sb.WriteString("Existing summary:\n")
	if strings.TrimSpace(existing) == "" {
		sb.WriteString("(none)\n")
	} else {
		sb.WriteString(existing)
		sb.WriteByte('\n')
	}
	sb.WriteString("\nNew turns:\n")
	for _, t := range turns {
		sb.WriteString(fmt.Sprintf("[%s] %s\n", t.Role, t.Content))
	}
	return sb.String()
}

// validateSummary rejects degenerate LLM output.
// LLMs occasionally refuse or return degenerate output. Fail loudly rather
// than corrupting the rolling summary with noise.
func validateSummary(s string) error {
	if len(s) < 20 {
		return fmt.Errorf("summary too short (%d chars)", len(s))
	}
	lower := strings.ToLower(s)
	for _, prefix := range []string{"i cannot", "i'm unable", "i don't"} {
		if strings.HasPrefix(lower, prefix) {
			end := min(50, len(s))
			return fmt.Errorf("summary appears to be a refusal: %q", s[:end])
		}
	}
	return nil
}

package reminder

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type claudeParser struct {
	client anthropic.Client
	logger *slog.Logger
}

// NewClaudeParser returns a Parser backed by the Anthropic API.
// The client is constructed once and reused — it holds an HTTP client
// internally and is safe for concurrent use.
func NewClaudeParser(apiKey string, logger *slog.Logger) Parser {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &claudeParser{client: client, logger: logger}
}

const claudeSystemPrompt = `You extract reminder details from a user's natural-language request.

Current time: %s
User's timezone: %s

Call the extract_reminder tool exactly once.

- If the text clearly specifies a time and a thing to be reminded of, fill in "when" (RFC3339 with offset, in the user's timezone) and "what" (a concise reminder body, imperative voice, no leading "to ").
- If the time is ambiguous, in the past, or the text isn't a reminder request, fill in "error" with a short, friendly message addressed directly to the user (e.g., "I couldn't figure out when you mean — try something like 'tomorrow at 6pm'"). Do not apologize or mention yourself.
- If the time is clear but the body is unintelligible (e.g. "remind me to blorgblop in 3 days"), fill in "error" explaining that the reminder body couldn't be understood (e.g., "I couldn't tell what you want to be reminded of — try something like 'brush my teeth'").
- Never fill in both a (when, what) pair and an error.`

var toolProperties = map[string]any{
	"when": map[string]any{
		"type":        "string",
		"description": "RFC3339 timestamp in the user's timezone, e.g. 2026-04-13T18:00:00-04:00",
	},
	"what": map[string]any{
		"type":        "string",
		"description": "Concise reminder body in imperative voice",
	},
	"error": map[string]any{
		"type":        "string",
		"description": "Set only if the request cannot be parsed into a reminder",
	},
}

type toolCallResult struct {
	When  string `json:"when"`
	What  string `json:"what"`
	Error string `json:"error"`
}

func (p *claudeParser) Parse(ctx context.Context, req ParseRequest) (ParsedReminder, error) {
	// Bound the call. Handler's ctx may have no deadline.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	system := fmt.Sprintf(
		claudeSystemPrompt,
		req.Now.Format(time.RFC3339),
		req.Loc.String(),
	)

	toolParam := anthropic.ToolParam{
		Name:        "extract_reminder",
		Description: anthropic.String("Return the parsed reminder or an error explanation."),
		InputSchema: anthropic.ToolInputSchemaParam{Properties: toolProperties},
	}

	resp, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 512,
		System: []anthropic.TextBlockParam{
			{Text: system},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Text)),
		},
		Tools: []anthropic.ToolUnionParam{
			{OfTool: &toolParam},
		},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: "extract_reminder"},
		},
	})
	if err != nil {
		return ParsedReminder{}, fmt.Errorf("claude: api call: %w", err)
	}

	var rawInput string
	for _, block := range resp.Content {
		if tu, ok := block.AsAny().(anthropic.ToolUseBlock); ok && tu.Name == "extract_reminder" {
			rawInput = tu.JSON.Input.Raw()
			break
		}
	}
	if rawInput == "" {
		return ParsedReminder{}, fmt.Errorf("claude: no tool_use block in response")
	}

	var result toolCallResult
	if err := json.Unmarshal([]byte(rawInput), &result); err != nil {
		return ParsedReminder{}, fmt.Errorf("claude: unmarshal tool input: %w", err)
	}

	if strings.TrimSpace(result.Error) != "" {
		p.logger.Debug("claude parse declined", "reason", result.Error, "text", req.Text)
		return ParsedReminder{}, newParseError(result.Error, nil)
	}

	when, err := time.Parse(time.RFC3339, result.When)
	if err != nil {
		return ParsedReminder{}, newParseError(
			"I understood your request but produced an invalid time. Please try rephrasing.",
			err,
		)
	}
	if strings.TrimSpace(result.What) == "" {
		return ParsedReminder{}, newParseError(
			"I couldn't figure out what to remind you about. Please try rephrasing.",
			nil,
		)
	}

	return ParsedReminder{When: when, What: strings.TrimSpace(result.What)}, nil
}

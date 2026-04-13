package fact

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/pmollerus23/reminders/internal/memory"
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

const factSystemPrompt = `You extract structured facts about a user from a natural-language statement.

Call the extract_fact tool exactly once.

Pick the best 'kind' for the statement:
  preference — a stated like or dislike (e.g. "I love black coffee", "I hate mornings")
  goal       — something the user is working toward (e.g. "I'm trying to run every day")
  person     — someone important to the user (e.g. "My wife Alice")
  routine    — a habitual activity at a regular time (e.g. "I meditate every morning")

Then fill 'content' with the matching shape:
  preference: {"topic": string, "detail": string}
  goal:       {"description": string, "cadence": string}  (cadence: "daily","weekly","monthly" or "")
  person:     {"name": string, "relation": string}
  routine:    {"description": string, "when": string}

If the statement is not a personal fact (e.g. it is a question or a reminder request),
set 'error' to a short friendly message explaining why it can't be stored as a fact.
Never fill both content and error.`

var factToolProperties = map[string]any{
	"kind": map[string]any{
		"type":        "string",
		"enum":        []string{"preference", "goal", "person", "routine"},
		"description": "Semantic category of the fact.",
	},
	"content": map[string]any{
		"type": "object",
		"description": `Structured content whose shape depends on kind:
preference: {"topic": string, "detail": string}
goal:       {"description": string, "cadence": string}
person:     {"name": string, "relation": string}
routine:    {"description": string, "when": string}`,
	},
	"error": map[string]any{
		"type":        "string",
		"description": "Set only if the text cannot be stored as a fact.",
	},
}

type factToolResult struct {
	Kind    string          `json:"kind"`
	Content json.RawMessage `json:"content"`
	Error   string          `json:"error"`
}

func (p *claudeParser) Parse(ctx context.Context, req ParseRequest) (ParsedFact, error) {
	// Bound the call. Handler's ctx may have no deadline.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	toolParam := anthropic.ToolParam{
		Name:        "extract_fact",
		Description: anthropic.String("Return the parsed fact or an error explanation."),
		InputSchema: anthropic.ToolInputSchemaParam{Properties: factToolProperties},
	}

	resp, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 512,
		System: []anthropic.TextBlockParam{
			{Text: factSystemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Text)),
		},
		Tools: []anthropic.ToolUnionParam{
			{OfTool: &toolParam},
		},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: "extract_fact"},
		},
	})
	if err != nil {
		return ParsedFact{}, fmt.Errorf("fact: api call: %w", err)
	}

	var rawInput string
	for _, block := range resp.Content {
		if tu, ok := block.AsAny().(anthropic.ToolUseBlock); ok && tu.Name == "extract_fact" {
			rawInput = tu.JSON.Input.Raw()
			break
		}
	}
	if rawInput == "" {
		return ParsedFact{}, fmt.Errorf("fact: no tool_use block in response")
	}

	var result factToolResult
	if err := json.Unmarshal([]byte(rawInput), &result); err != nil {
		return ParsedFact{}, fmt.Errorf("fact: unmarshal tool input: %w", err)
	}

	if strings.TrimSpace(result.Error) != "" {
		p.logger.Debug("fact parse declined", "reason", result.Error, "text", req.Text)
		return ParsedFact{}, newParseError(result.Error, nil)
	}

	kind := memory.Kind(result.Kind)

	// Validate that the content JSON matches the declared kind's shape.
	// We enforce the vocabulary at the write boundary — not just at read time.
	if err := validateContent(kind, result.Content); err != nil {
		p.logger.Debug("fact content shape mismatch", "kind", kind, "err", err)
		return ParsedFact{}, newParseError(
			"I couldn't understand that, try rephrasing.",
			err,
		)
	}

	return ParsedFact{Kind: kind, Content: result.Content}, nil
}

// validateContent checks that content decodes correctly into the schema
// declared for kind. This prevents malformed facts from reaching the store.
func validateContent(kind memory.Kind, content json.RawMessage) error {
	switch kind {
	case memory.KindPreference:
		var v struct {
			Topic  string `json:"topic"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(content, &v); err != nil {
			return fmt.Errorf("unmarshal preference: %w", err)
		}
		if v.Topic == "" || v.Detail == "" {
			return fmt.Errorf("preference: missing topic or detail")
		}
	case memory.KindGoal:
		var v struct {
			Description string `json:"description"`
			Cadence     string `json:"cadence"`
		}
		if err := json.Unmarshal(content, &v); err != nil {
			return fmt.Errorf("unmarshal goal: %w", err)
		}
		if v.Description == "" {
			return fmt.Errorf("goal: missing description")
		}
	case memory.KindPerson:
		var v struct {
			Name     string `json:"name"`
			Relation string `json:"relation"`
		}
		if err := json.Unmarshal(content, &v); err != nil {
			return fmt.Errorf("unmarshal person: %w", err)
		}
		if v.Name == "" {
			return fmt.Errorf("person: missing name")
		}
	case memory.KindRoutine:
		var v struct {
			Description string `json:"description"`
			When        string `json:"when"`
		}
		if err := json.Unmarshal(content, &v); err != nil {
			return fmt.Errorf("unmarshal routine: %w", err)
		}
		if v.Description == "" || v.When == "" {
			return fmt.Errorf("routine: missing description or when")
		}
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return nil
}

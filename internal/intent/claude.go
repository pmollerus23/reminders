package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/pmollerus23/reminders/internal/fact"
	"github.com/pmollerus23/reminders/internal/memory"
	"github.com/pmollerus23/reminders/internal/promptctx"
)

type claudeDispatcher struct {
	client anthropic.Client
	logger *slog.Logger
}

// NewClaudeDispatcher returns a Dispatcher backed by the Anthropic API.
// The client is constructed once and reused; it is safe for concurrent use.
func NewClaudeDispatcher(apiKey string, logger *slog.Logger) Dispatcher {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &claudeDispatcher{client: client, logger: logger}
}

// dispatchSystemSuffix is appended to the promptctx system block. It describes
// the three tools and provides the current time needed for reminder parsing.
// The {time} and {tz} placeholders are filled in by buildSystem.
const dispatchSystemSuffix = `

---
Call exactly one tool based on the user's message:
• set_reminder    — user wants to be reminded of something at a specific time
• remember_fact   — user is sharing a personal fact (preference, goal, person, or routine)
• chat_response   — everything else: questions, conversation, requests for information

Current time: %s
User's timezone: %s

Rules:
- set_reminder: fill "when" (RFC3339 with timezone offset) and "what" (imperative, no leading "to "). Optionally fill "recurrence" ("hourly", "daily", "weekly", or "monthly") if the user wants a repeating reminder. If the time is ambiguous, in the past, or the request is unclear, fill "error" with a friendly message instead.
- remember_fact: fill "kind" and "content" with the matching shape. If the text is not a storable personal fact, fill "error".
- chat_response: fill "message" with your reply. Use facts and conversation history from the context above.
- Never fill both a payload and an error in the same tool call.`

var (
	setReminderTool = anthropic.ToolParam{
		Name:        "set_reminder",
		Description: anthropic.String("Set a reminder for the user at a specific time, optionally recurring."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"when": map[string]any{
					"type":        "string",
					"description": "RFC3339 timestamp with timezone offset, e.g. 2026-04-13T18:00:00-04:00",
				},
				"what": map[string]any{
					"type":        "string",
					"description": "Concise reminder body in imperative voice",
				},
				"recurrence": map[string]any{
					"type":        "string",
					"enum":        []string{"hourly", "daily", "weekly", "monthly"},
					"description": "Repeat interval. Omit entirely for one-time reminders.",
				},
				"error": map[string]any{
					"type":        "string",
					"description": "Set only if the reminder cannot be parsed; friendly user-facing message",
				},
			},
		},
	}

	rememberFactTool = anthropic.ToolParam{
		Name:        "remember_fact",
		Description: anthropic.String("Store a personal fact about the user."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{"preference", "goal", "person", "routine"},
					"description": "Semantic category of the fact",
				},
				"content": map[string]any{
					"type": "object",
					"description": "Shape depends on kind:\n" +
						"  preference: {\"topic\": string, \"detail\": string}\n" +
						"  goal:       {\"description\": string, \"cadence\": string}\n" +
						"  person:     {\"name\": string, \"relation\": string}\n" +
						"  routine:    {\"description\": string, \"when\": string}",
				},
				"error": map[string]any{
					"type":        "string",
					"description": "Set only if the text is not a storable personal fact",
				},
			},
		},
	}

	chatResponseTool = anthropic.ToolParam{
		Name:        "chat_response",
		Description: anthropic.String("Respond to the user's message conversationally."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"message": map[string]any{
					"type":        "string",
					"description": "Your reply to the user",
				},
			},
		},
	}
)

func (d *claudeDispatcher) Dispatch(ctx context.Context, p promptctx.Prompt, now time.Time, loc *time.Location) (Action, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	system := p.System + fmt.Sprintf(dispatchSystemSuffix, now.Format(time.RFC3339), loc.String())

	messages := make([]anthropic.MessageParam, len(p.Messages))
	for i, m := range p.Messages {
		if m.Role == memory.RoleAssistant {
			messages[i] = anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content))
		} else {
			messages[i] = anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content))
		}
	}

	resp, err := d.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: system},
		},
		Messages: messages,
		Tools: []anthropic.ToolUnionParam{
			{OfTool: &setReminderTool},
			{OfTool: &rememberFactTool},
			{OfTool: &chatResponseTool},
		},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfAny: &anthropic.ToolChoiceAnyParam{},
		},
	})
	if err != nil {
		return Action{}, fmt.Errorf("intent: api call: %w", err)
	}

	for _, block := range resp.Content {
		tu, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		rawInput := tu.JSON.Input.Raw()

		switch tu.Name {
		case "set_reminder":
			return d.parseSetReminder(rawInput)
		case "remember_fact":
			return d.parseRememberFact(rawInput)
		case "chat_response":
			return d.parseChatResponse(rawInput)
		}
	}

	return Action{}, fmt.Errorf("intent: no tool_use block in response")
}

type reminderInput struct {
	When       string `json:"when"`
	What       string `json:"what"`
	Recurrence string `json:"recurrence"` // optional; empty means one-time
	Error      string `json:"error"`
}

func (d *claudeDispatcher) parseSetReminder(raw string) (Action, error) {
	var in reminderInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return Action{}, fmt.Errorf("intent: unmarshal set_reminder: %w", err)
	}
	if strings.TrimSpace(in.Error) != "" {
		d.logger.Debug("intent: set_reminder declined", "reason", in.Error)
		return Action{}, &DispatchError{UserMessage: in.Error}
	}
	if _, err := time.Parse(time.RFC3339, in.When); err != nil {
		return Action{}, &DispatchError{
			UserMessage: "I understood your request but produced an invalid time. Please try rephrasing.",
		}
	}
	if strings.TrimSpace(in.What) == "" {
		return Action{}, &DispatchError{
			UserMessage: "I couldn't figure out what to remind you about. Please try rephrasing.",
		}
	}
	a := Action{Kind: ActionSetReminder, ReminderWhen: in.When, ReminderWhat: strings.TrimSpace(in.What)}
	if r := strings.TrimSpace(in.Recurrence); r != "" {
		a.ReminderRecurrence = &r
	}
	return a, nil
}

type factInput struct {
	Kind    string          `json:"kind"`
	Content json.RawMessage `json:"content"`
	Error   string          `json:"error"`
}

func (d *claudeDispatcher) parseRememberFact(raw string) (Action, error) {
	var in factInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return Action{}, fmt.Errorf("intent: unmarshal remember_fact: %w", err)
	}
	if strings.TrimSpace(in.Error) != "" {
		d.logger.Debug("intent: remember_fact declined", "reason", in.Error)
		return Action{}, &DispatchError{UserMessage: in.Error}
	}
	kind := memory.Kind(in.Kind)
	if err := fact.ValidateContent(kind, in.Content); err != nil {
		d.logger.Debug("intent: remember_fact content mismatch", "kind", kind, "err", err)
		return Action{}, &DispatchError{UserMessage: "I couldn't understand that, try rephrasing."}
	}
	return Action{Kind: ActionRememberFact, FactKind: kind, FactContent: in.Content}, nil
}

type chatInput struct {
	Message string `json:"message"`
}

func (d *claudeDispatcher) parseChatResponse(raw string) (Action, error) {
	var in chatInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return Action{}, fmt.Errorf("intent: unmarshal chat_response: %w", err)
	}
	if strings.TrimSpace(in.Message) == "" {
		return Action{}, fmt.Errorf("intent: empty chat_response message")
	}
	return Action{Kind: ActionChat, ChatReply: in.Message}, nil
}

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
// the tools and the agentic loop termination condition.
const dispatchSystemSuffix = `

---
You are running in an agentic loop. Call tools as needed to fulfill the user's request, then ALWAYS end by calling chat_response to send your reply.

Tools available:
• set_reminder      — create a new reminder at a specific time (optionally recurring)
• update_reminder   — change the time, body, or recurrence of an existing reminder (use list_reminders first to get the ID)
• delete_reminder   — remove a reminder by ID
• list_reminders    — retrieve all pending reminders for this user
• remember_fact     — store a new personal fact (preference, goal, person, routine)
• update_fact       — change the content of a stored fact (use list_facts first to get the ID)
• delete_fact       — remove a fact by ID
• list_facts        — retrieve all stored facts for this user
• chat_response     — send your final reply; ALWAYS call this last

Current time: %s
User's timezone: %s

Rules:
- set_reminder: fill "when" (RFC3339 with timezone offset) and "what" (imperative). Optionally fill "recurrence" for repeating reminders. Fill "error" if the time is ambiguous or unclear.
- update_reminder: requires "id" (from list_reminders), "what", and "when". "recurrence" is optional — omit for a one-time reminder.
- delete_reminder / delete_fact: requires "id" only.
- list_reminders / list_facts: no parameters required.
- remember_fact: fill "kind" and "content". Fill "error" if the input isn't a storable personal fact.
- update_fact: requires "id" (from list_facts), "kind", and "content".
- chat_response: always call this to deliver your final message, incorporating any tool results.
- Never fill both a payload and an error in the same tool call.`

// maxAgentIterations caps the number of Claude API calls in a single Dispatch
// invocation to prevent infinite loops.
const maxAgentIterations = 10

var (
	setReminderTool = anthropic.ToolParam{
		Name:        "set_reminder",
		Description: anthropic.String("Create a new reminder at a specific time, optionally recurring."),
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
					"description": "Repeat interval. Omit for one-time reminders.",
				},
				"error": map[string]any{
					"type":        "string",
					"description": "Set if the reminder cannot be parsed; friendly user-facing message",
				},
			},
		},
	}

	updateReminderTool = anthropic.ToolParam{
		Name:        "update_reminder",
		Description: anthropic.String("Change the body, time, or recurrence of an existing pending reminder. Call list_reminders first to get the ID."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "UUID of the reminder to update (from list_reminders)",
				},
				"what": map[string]any{
					"type":        "string",
					"description": "New reminder body in imperative voice",
				},
				"when": map[string]any{
					"type":        "string",
					"description": "New RFC3339 timestamp with timezone offset",
				},
				"recurrence": map[string]any{
					"type":        "string",
					"enum":        []string{"hourly", "daily", "weekly", "monthly"},
					"description": "New repeat interval. Omit to make the reminder one-time.",
				},
				"error": map[string]any{
					"type":        "string",
					"description": "Set if the update cannot be completed; friendly user-facing message",
				},
			},
		},
	}

	deleteReminderTool = anthropic.ToolParam{
		Name:        "delete_reminder",
		Description: anthropic.String("Remove a reminder permanently. Call list_reminders first to get the ID."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "UUID of the reminder to delete",
				},
			},
		},
	}

	listRemindersTool = anthropic.ToolParam{
		Name:        "list_reminders",
		Description: anthropic.String("Retrieve all pending reminders for this user. Returns a JSON array with id, what, when, and recurrence fields."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{},
		},
	}

	rememberFactTool = anthropic.ToolParam{
		Name:        "remember_fact",
		Description: anthropic.String("Store a new personal fact about the user."),
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
					"description": "Set if the text is not a storable personal fact",
				},
			},
		},
	}

	updateFactTool = anthropic.ToolParam{
		Name:        "update_fact",
		Description: anthropic.String("Replace the content of a stored fact. Call list_facts first to get the ID."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "UUID of the fact to update (from list_facts)",
				},
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{"preference", "goal", "person", "routine"},
					"description": "Semantic category of the updated fact",
				},
				"content": map[string]any{
					"type":        "object",
					"description": "New content; shape must match the kind",
				},
			},
		},
	}

	deleteFactTool = anthropic.ToolParam{
		Name:        "delete_fact",
		Description: anthropic.String("Remove a stored fact permanently. Call list_facts first to get the ID."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "UUID of the fact to delete",
				},
			},
		},
	}

	listFactsTool = anthropic.ToolParam{
		Name:        "list_facts",
		Description: anthropic.String("Retrieve all stored facts for this user. Returns a JSON array with id, kind, and content fields."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{},
		},
	}

	chatResponseTool = anthropic.ToolParam{
		Name:        "chat_response",
		Description: anthropic.String("Send the final reply to the user. Always call this last."),
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

// allTools is the full toolset passed to every API call.
var allTools = []anthropic.ToolUnionParam{
	{OfTool: &setReminderTool},
	{OfTool: &updateReminderTool},
	{OfTool: &deleteReminderTool},
	{OfTool: &listRemindersTool},
	{OfTool: &rememberFactTool},
	{OfTool: &updateFactTool},
	{OfTool: &deleteFactTool},
	{OfTool: &listFactsTool},
	{OfTool: &chatResponseTool},
}

// Dispatch runs an agentic loop: it calls Claude repeatedly, executing any
// tool calls via the provided ToolSet, until the model calls chat_response
// to deliver its final reply.
func (d *claudeDispatcher) Dispatch(
	ctx context.Context,
	p promptctx.Prompt,
	now time.Time,
	loc *time.Location,
	tools ToolSet,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
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

	for iter := range maxAgentIterations {
		resp, err := d.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeHaiku4_5,
			MaxTokens: 1024,
			System: []anthropic.TextBlockParam{
				{Text: system},
			},
			Messages: messages,
			Tools:    allTools,
			ToolChoice: anthropic.ToolChoiceUnionParam{
				OfAny: &anthropic.ToolChoiceAnyParam{},
			},
		})
		if err != nil {
			return "", fmt.Errorf("intent: api call (iter %d): %w", iter, err)
		}

		d.logger.Debug("agentic loop iteration",
			"iter", iter,
			"stop_reason", resp.StopReason,
			"tool_calls", countToolUse(resp.Content),
		)

		// Collect all tool_use blocks from this response turn.
		var toolCalls []anthropic.ToolUseBlock
		for _, block := range resp.Content {
			if tu, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
				toolCalls = append(toolCalls, tu)
			}
		}

		// No tool calls — try to extract a text block (end_turn path).
		if len(toolCalls) == 0 {
			for _, block := range resp.Content {
				if tb, ok := block.AsAny().(anthropic.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
					return tb.Text, nil
				}
			}
			return "", fmt.Errorf("intent: no tool calls or text in response (iter %d)", iter)
		}

		// Build the assistant content param list from the response content blocks
		// so we can feed it back in the next turn's message history.
		assistantContent := make([]anthropic.ContentBlockParamUnion, 0, len(resp.Content))
		for _, block := range resp.Content {
			switch b := block.AsAny().(type) {
			case anthropic.TextBlock:
				assistantContent = append(assistantContent, anthropic.NewTextBlock(b.Text))
			case anthropic.ToolUseBlock:
				assistantContent = append(assistantContent,
					anthropic.NewToolUseBlock(b.ID, json.RawMessage(b.JSON.Input.Raw()), b.Name))
			}
		}

		// Execute tool calls and collect results. chat_response terminates the loop.
		toolResults := make([]anthropic.ContentBlockParamUnion, 0, len(toolCalls))
		for _, tu := range toolCalls {
			if tu.Name == "chat_response" {
				var in chatInput
				if err := json.Unmarshal([]byte(tu.JSON.Input.Raw()), &in); err != nil {
					return "", fmt.Errorf("intent: unmarshal chat_response: %w", err)
				}
				reply := strings.TrimSpace(in.Message)
				if reply == "" {
					return "", fmt.Errorf("intent: empty chat_response message")
				}
				return reply, nil
			}

			result, isError := d.executeTool(ctx, tu, tools)
			if isError {
				// Check if this is an infrastructure error (wrapped by executeTool
				// when the ToolSet callback itself fails). Propagate immediately
				// rather than feeding DB errors back to the model.
				if msg, ok := strings.CutPrefix(result, "INFRA:"); ok {
					return "", fmt.Errorf("intent: tool %q: %s", tu.Name, msg)
				}
			}
			toolResults = append(toolResults,
				anthropic.NewToolResultBlock(tu.ID, result, isError))
		}

		// Append assistant turn and tool results, then loop for the model's next turn.
		messages = append(messages, anthropic.NewAssistantMessage(assistantContent...))
		messages = append(messages, anthropic.NewUserMessage(toolResults...))
	}

	return "", fmt.Errorf("intent: agentic loop exceeded %d iterations", maxAgentIterations)
}

// executeTool dispatches a single tool_use block to the appropriate ToolSet
// callback. Returns (resultText, isError).
//
// isError=true + result prefix "INFRA:" signals an infrastructure failure that
// should be propagated up rather than fed back to the model.
// isError=true without that prefix is a logical/user error the model can handle.
func (d *claudeDispatcher) executeTool(
	ctx context.Context,
	tu anthropic.ToolUseBlock,
	tools ToolSet,
) (result string, isError bool) {
	raw := tu.JSON.Input.Raw()

	switch tu.Name {
	// ── Reminder tools ───────────────────────────────────────────────────────

	case "set_reminder":
		var in reminderInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return fmt.Sprintf("parse error: %v", err), true
		}
		if msg := strings.TrimSpace(in.Error); msg != "" {
			d.logger.Debug("set_reminder declined by model", "reason", msg)
			return msg, true
		}
		when, err := time.Parse(time.RFC3339, in.When)
		if err != nil {
			return "invalid time format", true
		}
		var recurrence *string
		if r := strings.TrimSpace(in.Recurrence); r != "" {
			recurrence = &r
		}
		msg, err := tools.SetReminder(ctx, strings.TrimSpace(in.What), when, recurrence)
		if err != nil {
			d.logger.Warn("set_reminder callback failed", "err", err)
			return "INFRA:" + err.Error(), true
		}
		return msg, false

	case "update_reminder":
		var in updateReminderInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return fmt.Sprintf("parse error: %v", err), true
		}
		if msg := strings.TrimSpace(in.Error); msg != "" {
			return msg, true
		}
		when, err := time.Parse(time.RFC3339, in.When)
		if err != nil {
			return "invalid time format", true
		}
		var recurrence *string
		if r := strings.TrimSpace(in.Recurrence); r != "" {
			recurrence = &r
		}
		msg, err := tools.UpdateReminder(ctx, in.ID, strings.TrimSpace(in.What), when, recurrence)
		if err != nil {
			d.logger.Warn("update_reminder callback failed", "err", err)
			return "INFRA:" + err.Error(), true
		}
		return msg, false

	case "delete_reminder":
		var in idInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return fmt.Sprintf("parse error: %v", err), true
		}
		msg, err := tools.DeleteReminder(ctx, in.ID)
		if err != nil {
			d.logger.Warn("delete_reminder callback failed", "err", err)
			return "INFRA:" + err.Error(), true
		}
		return msg, false

	case "list_reminders":
		reminders, err := tools.ListReminders(ctx)
		if err != nil {
			d.logger.Warn("list_reminders callback failed", "err", err)
			return "INFRA:" + err.Error(), true
		}
		data, err := json.Marshal(reminders)
		if err != nil {
			return "INFRA:marshal list_reminders: " + err.Error(), true
		}
		if len(reminders) == 0 {
			return "No pending reminders.", false
		}
		return string(data), false

	// ── Fact tools ───────────────────────────────────────────────────────────

	case "remember_fact":
		var in factInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return fmt.Sprintf("parse error: %v", err), true
		}
		if msg := strings.TrimSpace(in.Error); msg != "" {
			d.logger.Debug("remember_fact declined by model", "reason", msg)
			return msg, true
		}
		kind := memory.Kind(in.Kind)
		if err := fact.ValidateContent(kind, in.Content); err != nil {
			d.logger.Debug("remember_fact content mismatch", "kind", kind, "err", err)
			return "content shape mismatch — could not store fact", true
		}
		msg, err := tools.RememberFact(ctx, kind, in.Content)
		if err != nil {
			d.logger.Warn("remember_fact callback failed", "err", err)
			return "INFRA:" + err.Error(), true
		}
		return msg, false

	case "update_fact":
		var in updateFactInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return fmt.Sprintf("parse error: %v", err), true
		}
		kind := memory.Kind(in.Kind)
		if err := fact.ValidateContent(kind, in.Content); err != nil {
			return "content shape mismatch — could not update fact", true
		}
		msg, err := tools.UpdateFact(ctx, in.ID, kind, in.Content)
		if err != nil {
			d.logger.Warn("update_fact callback failed", "err", err)
			return "INFRA:" + err.Error(), true
		}
		return msg, false

	case "delete_fact":
		var in idInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return fmt.Sprintf("parse error: %v", err), true
		}
		msg, err := tools.DeleteFact(ctx, in.ID)
		if err != nil {
			d.logger.Warn("delete_fact callback failed", "err", err)
			return "INFRA:" + err.Error(), true
		}
		return msg, false

	case "list_facts":
		facts, err := tools.ListFacts(ctx)
		if err != nil {
			d.logger.Warn("list_facts callback failed", "err", err)
			return "INFRA:" + err.Error(), true
		}
		data, err := json.Marshal(facts)
		if err != nil {
			return "INFRA:marshal list_facts: " + err.Error(), true
		}
		if len(facts) == 0 {
			return "No stored facts.", false
		}
		return string(data), false

	default:
		return fmt.Sprintf("unknown tool %q", tu.Name), true
	}
}

// countToolUse counts ToolUseBlocks in a content slice for debug logging.
func countToolUse(content []anthropic.ContentBlockUnion) int {
	n := 0
	for _, b := range content {
		if _, ok := b.AsAny().(anthropic.ToolUseBlock); ok {
			n++
		}
	}
	return n
}

// ── Tool input structs ────────────────────────────────────────────────────────

type reminderInput struct {
	When       string `json:"when"`
	What       string `json:"what"`
	Recurrence string `json:"recurrence"`
	Error      string `json:"error"`
}

type updateReminderInput struct {
	ID         string `json:"id"`
	What       string `json:"what"`
	When       string `json:"when"`
	Recurrence string `json:"recurrence"`
	Error      string `json:"error"`
}

type factInput struct {
	Kind    string          `json:"kind"`
	Content json.RawMessage `json:"content"`
	Error   string          `json:"error"`
}

type updateFactInput struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Content json.RawMessage `json:"content"`
}

type idInput struct {
	ID string `json:"id"`
}

type chatInput struct {
	Message string `json:"message"`
}

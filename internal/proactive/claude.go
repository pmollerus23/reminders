package proactive

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

type claudeAgent struct {
	client anthropic.Client
	logger *slog.Logger
}

// NewClaudeAgent returns an Agent backed by the Anthropic API.
func NewClaudeAgent(apiKey string, logger *slog.Logger) Agent {
	return &claudeAgent{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
		logger: logger,
	}
}

// Consider calls Claude with the user's profile and asks it to decide whether
// to send a proactive message. Returns the message text, or "" to stay silent.
func (a *claudeAgent) Consider(ctx context.Context, req ConsiderRequest) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	system := buildSystemPrompt(req)

	sendTool := anthropic.ToolParam{
		Name:        "send_message",
		Description: anthropic.String("Send a proactive message to the user."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"message": map[string]any{
					"type":        "string",
					"description": "The message to send. Keep it brief and specific.",
				},
			},
		},
	}

	skipTool := anthropic.ToolParam{
		Name:        "skip",
		Description: anthropic.String("Stay silent — nothing worth saying right now."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{},
		},
	}

	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 512,
		System: []anthropic.TextBlockParam{
			{Text: system},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("Review this user's profile and decide whether to reach out.")),
		},
		Tools: []anthropic.ToolUnionParam{
			{OfTool: &sendTool},
			{OfTool: &skipTool},
		},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfAny: &anthropic.ToolChoiceAnyParam{},
		},
	})
	if err != nil {
		return "", fmt.Errorf("proactive: api call: %w", err)
	}

	for _, block := range resp.Content {
		tu, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		switch tu.Name {
		case "send_message":
			var in struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(tu.JSON.Input.Raw()), &in); err != nil {
				return "", fmt.Errorf("proactive: unmarshal send_message: %w", err)
			}
			msg := strings.TrimSpace(in.Message)
			if msg == "" {
				return "", fmt.Errorf("proactive: empty send_message")
			}
			a.logger.Debug("proactive agent chose to send", "preview", truncate(msg, 60))
			return msg, nil
		case "skip":
			return "", nil
		}
	}

	return "", fmt.Errorf("proactive: no tool_use block in response")
}

// buildSystemPrompt assembles the full system prompt from the user's profile.
func buildSystemPrompt(req ConsiderRequest) string {
	var sb strings.Builder

	sb.WriteString(`You are a thoughtful personal assistant. Review the user's profile below and decide whether there is something genuinely worth telling them right now.

Call send_message if there is something specific and helpful to say. Good reasons:
• A goal they mentioned — offer a timely nudge or check-in
• A reminder due today or tomorrow — help them prepare
• A useful observation based on their preferences or routines

Call skip if there is nothing worth saying. Bad reasons to reach out:
• Generic greetings with no substance ("How are you?", "Just checking in!")
• Repeating information the user already knows
• Filling silence for its own sake

Keep messages brief, specific, and warm. Never apologize for reaching out.

`)

	fmt.Fprintf(&sb, "Current time: %s\nTimezone: %s\n\n",
		req.Now.Format(time.RFC3339), req.Loc.String())

	// Facts
	sb.WriteString("## Stored facts about the user\n")
	if len(req.Facts) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, f := range req.Facts {
			fmt.Fprintf(&sb, "- [%s] %s\n", f.Kind, string(f.Content))
		}
	}
	sb.WriteString("\n")

	// Upcoming reminders
	sb.WriteString("## Upcoming reminders (next 7 days)\n")
	if len(req.Reminders) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, r := range req.Reminders {
			line := fmt.Sprintf("- %s at %s",
				r.Body, r.ScheduledAt.In(req.Loc).Format("Mon Jan 2 15:04 MST"))
			if r.Recurrence != nil {
				line += fmt.Sprintf(" (repeats %s)", *r.Recurrence)
			}
			sb.WriteString(line + "\n")
		}
	}
	sb.WriteString("\n")

	// Recent conversation
	sb.WriteString("## Recent conversation (last 10 turns)\n")
	if len(req.RecentTurns) == 0 {
		sb.WriteString("(no recent conversation)\n")
	} else {
		for _, t := range req.RecentTurns {
			fmt.Fprintf(&sb, "%s: %s\n", t.Role, t.Content)
		}
	}

	return sb.String()
}

// truncate shortens s to at most n runes for logging.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}


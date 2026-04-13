// Package promptctx assembles LLM prompts from stored memory.
// It depends on the memory package for types but not on any Anthropic SDK types,
// so callers can use it with any model client.
package promptctx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/pmollerus23/reminders/internal/memory"
)

// Message is one turn to pass to the LLM. It mirrors memory.Turn but lives
// in this package so prompt assembly stays decoupled from storage types.
type Message struct {
	Role    memory.Role
	Content string
}

// Prompt is the fully-assembled input for one LLM call.
type Prompt struct {
	System   string
	Messages []Message
}

// Builder assembles Prompts from stored memory plus live user input.
type Builder struct {
	store         memory.Store
	systemPrompt  string
	verbatimTurns int // how many recent turns to include verbatim
}

// New creates a Builder.
// verbatimTurns controls how many recent turns appear in Messages; turns older
// than that are expected to be folded into the chat summary by the summarize loop.
func New(store memory.Store, systemPrompt string, verbatimTurns int) *Builder {
	return &Builder{
		store:         store,
		systemPrompt:  systemPrompt,
		verbatimTurns: verbatimTurns,
	}
}

// systemTmpl renders the system block:
//   - .Base is always present
//   - .Facts renders only when the slice is non-empty
//   - .Summary renders only when non-empty
//
// text/template is used (not html/template) because the output goes to an LLM,
// not a browser — no HTML escaping needed or wanted.
const systemTmpl = `{{.Base}}{{if .Facts}}

## What I know about you
{{range .Facts}}- {{.}}
{{end}}{{- end}}{{if .Summary}}

## Conversation history (summary)
{{.Summary}}{{end}}`

type systemData struct {
	Base    string
	Facts   []string // pre-rendered fact lines; nil means "omit section"
	Summary string   // empty means "omit section"
}

// BuildChatPrompt loads facts, summary, and recent turns for chatID, renders
// the system block, and appends userInput as the final user message.
func (b *Builder) BuildChatPrompt(ctx context.Context, chatID int64, userInput string) (Prompt, error) {
	facts, err := b.store.Facts(ctx, chatID)
	if err != nil {
		return Prompt{}, fmt.Errorf("promptctx: load facts: %w", err)
	}

	summary, err := b.store.GetSummary(ctx, chatID)
	if err != nil {
		return Prompt{}, fmt.Errorf("promptctx: load summary: %w", err)
	}

	turns, err := b.store.RecentTurns(ctx, chatID, b.verbatimTurns)
	if err != nil {
		return Prompt{}, fmt.Errorf("promptctx: load turns: %w", err)
	}

	var rendered []string
	for _, f := range facts {
		s, err := renderFact(f)
		if err != nil {
			return Prompt{}, fmt.Errorf("promptctx: render fact %s: %w", f.ID, err)
		}
		rendered = append(rendered, s)
	}

	data := systemData{Base: b.systemPrompt}
	if len(rendered) > 0 {
		data.Facts = rendered
	}
	if summary != nil {
		data.Summary = summary.Text
	}

	tmpl, err := template.New("system").Parse(systemTmpl)
	if err != nil {
		// The template is a compile-time constant; this would be a programming error.
		return Prompt{}, fmt.Errorf("promptctx: parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return Prompt{}, fmt.Errorf("promptctx: execute template: %w", err)
	}

	messages := make([]Message, 0, len(turns)+1)
	for _, t := range turns {
		messages = append(messages, Message{Role: t.Role, Content: t.Content})
	}
	messages = append(messages, Message{Role: memory.RoleUser, Content: userInput})

	return Prompt{
		System:   buf.String(),
		Messages: messages,
	}, nil
}

// renderFact converts a Fact into a single human-readable line.
// Each Kind has a declared content shape (an inline struct) matching the JSON
// stored by whoever wrote the fact. Unknown Kinds fall back to raw JSON rather
// than being silently dropped.
func renderFact(f memory.Fact) (string, error) {
	switch f.Kind {
	case memory.KindPreference:
		var v struct {
			Topic  string `json:"topic"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(f.Content, &v); err != nil {
			return "", fmt.Errorf("unmarshal preference: %w", err)
		}
		return fmt.Sprintf("Preference — %s: %s", v.Topic, v.Detail), nil

	case memory.KindGoal:
		var v struct {
			Description string `json:"description"`
			Cadence     string `json:"cadence"`
		}
		if err := json.Unmarshal(f.Content, &v); err != nil {
			return "", fmt.Errorf("unmarshal goal: %w", err)
		}
		if v.Cadence != "" {
			return fmt.Sprintf("Goal (%s): %s", v.Cadence, v.Description), nil
		}
		return fmt.Sprintf("Goal: %s", v.Description), nil

	case memory.KindPerson:
		var v struct {
			Name     string `json:"name"`
			Relation string `json:"relation"`
		}
		if err := json.Unmarshal(f.Content, &v); err != nil {
			return "", fmt.Errorf("unmarshal person: %w", err)
		}
		return fmt.Sprintf("Person — %s (%s)", v.Name, v.Relation), nil

	case memory.KindRoutine:
		var v struct {
			Description string `json:"description"`
			When        string `json:"when"`
		}
		if err := json.Unmarshal(f.Content, &v); err != nil {
			return "", fmt.Errorf("unmarshal routine: %w", err)
		}
		return fmt.Sprintf("Routine — %s: %s", v.When, v.Description), nil

	default:
		// TODO: Add a case when a new Kind constant is added to memory.go.
		// Falling back to raw JSON ensures facts are never silently lost.
		return fmt.Sprintf("(%s) %s", f.Kind, strings.TrimSpace(string(f.Content))), nil
	}
}

package fact

import (
	"encoding/json"
	"fmt"

	"github.com/pmollerus23/reminders/internal/memory"
)

// ValidateContent checks that content decodes correctly into the schema
// declared for kind. This enforces the fact vocabulary at the write boundary
// rather than only at read time. Called by both the fact parser and the
// intent dispatcher.
func ValidateContent(kind memory.Kind, content json.RawMessage) error {
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

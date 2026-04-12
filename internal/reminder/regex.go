package reminder

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// regexParser implements Parser using a strict positional format:
// "YYYY-MM-DD HH:MM body". Kept as the offline/dev parser and the
// default test double — no API calls, no non-determinism.
type regexParser struct{}

// NewRegexParser returns a Parser backed by strict positional parsing.
// Returned as the interface type because callers have no reason to
// depend on the concrete struct.
func NewRegexParser() Parser {
	return &regexParser{}
}

const layout = "2006-01-02 15:04"

func (p *regexParser) Parse(_ context.Context, req ParseRequest) (ParsedReminder, error) {
	text := strings.TrimSpace(req.Text)

	// Expect "YYYY-MM-DD HH:MM body...". SplitN with n=3 keeps the body
	// intact even if it contains spaces.
	parts := strings.SplitN(text, " ", 3)
	if len(parts) < 3 {
		return ParsedReminder{}, fmt.Errorf("%w: expected 'YYYY-MM-DD HH:MM body'", ErrBadFormat)
	}

	dateStr, timeStr, body := parts[0], parts[1], parts[2]

	when, err := time.ParseInLocation(layout, dateStr+" "+timeStr, req.Loc)
	if err != nil {
		return ParsedReminder{}, fmt.Errorf("%w: %v", ErrBadFormat, err)
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return ParsedReminder{}, fmt.Errorf("%w: reminder body is empty", ErrBadFormat)
	}

	return ParsedReminder{When: when, What: body}, nil
}

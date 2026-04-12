package reminder

import (
	"context"
	"strings"
	"time"
)

// regexParser implements Parser using a strict positional format:
// "YYYY-MM-DD HH:MM body". Kept as the offline/dev parser and the
// default test double — no API calls, no non-determinism.
type regexParser struct{}

// NewRegexParser returns a Parser backed by strict positional parsing.
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
		return ParsedReminder{}, newParseError(
			"Please use: YYYY-MM-DD HH:MM <reminder text>",
			nil,
		)
	}

	dateStr, timeStr, body := parts[0], parts[1], parts[2]

	when, err := time.ParseInLocation(layout, dateStr+" "+timeStr, req.Loc)
	if err != nil {
		return ParsedReminder{}, newParseError(
			"I couldn't parse that date and time. Try: 2026-04-13 18:00",
			err,
		)
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return ParsedReminder{}, newParseError(
			"Your reminder is missing a body.",
			nil,
		)
	}

	return ParsedReminder{When: when, What: body}, nil
}

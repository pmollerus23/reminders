package reminder

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ParsedReminder is the structured result of parsing a user's command.
// It is deliberately minimal: just the two fields a reminder row needs
// beyond the chat ID (which comes from the update envelope, not the text).
type ParsedReminder struct {
	When time.Time
	What string
}

// Sentinel errors let callers distinguish "not my problem" from "malformed".
// ErrNotACommand means the text isn't a /remind command at all — the handler
// should silently ignore it. ErrBadFormat means the user tried to issue a
// command but got the syntax wrong — the handler should reply with usage.
var (
	ErrNotACommand = errors.New("not a command")
	ErrBadFormat   = errors.New("bad command format")
)

const commandPrefix = "/remind"

// layout is the one date-time format we accept for now. Keep it strict;
// loosening later is easy, tightening later breaks users.
const layout = "2006-01-02 15:04"

// ParseCommand turns raw message text into a ParsedReminder.
//
// Expected form: "/remind 2026-04-12 18:00 body text here"
//
// Times are parsed in the server's local timezone. Per-user timezones
// are a future concern; flagging here so we don't forget.
func ParseCommand(text string) (ParsedReminder, error) {
	text = strings.TrimSpace(text)

	// SplitN with n=4 gives us at most 4 fields, keeping the body intact
	// even if it contains spaces. This is cleaner than regex for positional
	// parsing — regex earns its keep when you need validation patterns,
	// not when you're just splitting on whitespace.
	parts := strings.SplitN(text, " ", 4)

	if len(parts) == 0 || parts[0] != commandPrefix {
		return ParsedReminder{}, ErrNotACommand
	}

	if len(parts) < 4 {
		return ParsedReminder{}, fmt.Errorf("%w: expected '/remind YYYY-MM-DD HH:MM body'", ErrBadFormat)
	}

	dateStr, timeStr, body := parts[1], parts[2], parts[3]

	// ParseInLocation, not Parse. time.Parse assumes UTC when the layout
	// has no zone, which would silently shift reminders by the server's
	// offset. time.Local makes the assumption explicit.
	when, err := time.ParseInLocation(layout, dateStr+" "+timeStr, time.Local)
	if err != nil {
		return ParsedReminder{}, fmt.Errorf("%w: %v", ErrBadFormat, err)
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return ParsedReminder{}, fmt.Errorf("%w: reminder body is empty", ErrBadFormat)
	}

	return ParsedReminder{When: when, What: body}, nil
}

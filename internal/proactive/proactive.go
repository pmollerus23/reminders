// Package proactive implements the spontaneous-messaging loop. On each tick it
// finds users who have stored facts, have been active recently, and haven't
// received a proactive message within the configured interval. For each
// eligible user it asks the Agent whether to send something; the Agent decides
// based on the user's profile and current time.
package proactive

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pmollerus23/reminders/internal/db"
	"github.com/pmollerus23/reminders/internal/memory"
)

// Messenger is the outbound side of a messaging platform. Defined here,
// consumer-side, following the project convention.
type Messenger interface {
	Send(ctx context.Context, chatID int64, body string) error
}

// Agent decides whether to send a proactive message to a user. Consider
// returns the message text to send, or "" to stay silent.
type Agent interface {
	Consider(ctx context.Context, req ConsiderRequest) (string, error)
}

// ConsiderRequest is the user profile snapshot handed to the Agent.
type ConsiderRequest struct {
	Facts       []memory.Fact
	Reminders   []db.Reminder
	RecentTurns []memory.Turn
	Now         time.Time
	Loc         *time.Location
}

// checkInterval is how often the loop polls for eligible users.
// Deliberately separate from proactiveInterval (per-user min gap):
// the loop wakes up hourly but only messages users whose last contact
// is older than proactiveInterval.
const checkInterval = time.Hour

// quietStart / quietEnd define the window (in the configured timezone)
// during which proactive messages are allowed. Outside this window the
// loop skips the tick entirely.
const (
	quietStart = 8  // 08:00 local
	quietEnd   = 21 // 21:00 local
)

// Loop runs the proactive messaging cadence.
type Loop struct {
	pool               *pgxpool.Pool
	mem                memory.Store
	msgr               Messenger
	agent              Agent
	proactiveInterval  time.Duration
	loc                *time.Location
	logger             *slog.Logger
}

// NewLoop constructs a Loop. proactiveInterval is the minimum time between
// proactive messages per user (e.g. 24h).
func NewLoop(
	pool *pgxpool.Pool,
	mem memory.Store,
	msgr Messenger,
	agent Agent,
	proactiveInterval time.Duration,
	loc *time.Location,
	logger *slog.Logger,
) *Loop {
	return &Loop{
		pool:              pool,
		mem:               mem,
		msgr:              msgr,
		agent:             agent,
		proactiveInterval: proactiveInterval,
		loc:               loc,
		logger:            logger,
	}
}

// Run blocks until ctx is cancelled, ticking every checkInterval.
func (l *Loop) Run(ctx context.Context) error {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	l.logger.Info("proactive loop started",
		"check_interval", checkInterval,
		"proactive_interval", l.proactiveInterval,
	)

	for {
		select {
		case <-ctx.Done():
			l.logger.Info("proactive loop stopped")
			return nil
		case <-ticker.C:
			if err := l.tick(ctx); err != nil {
				l.logger.Error("proactive tick failed", "err", err)
			}
		}
	}
}

func (l *Loop) tick(ctx context.Context) error {
	now := time.Now().In(l.loc)
	hour := now.Hour()
	if hour < quietStart || hour >= quietEnd {
		return nil // outside quiet window
	}

	chatIDs, err := l.eligibleChats(ctx)
	if err != nil {
		return fmt.Errorf("proactive: eligible chats: %w", err)
	}
	if len(chatIDs) == 0 {
		return nil
	}

	l.logger.Debug("proactive: checking eligible chats", "count", len(chatIDs))

	for _, chatID := range chatIDs {
		if err := l.considerAndSend(ctx, chatID, now); err != nil {
			l.logger.Error("proactive: consider failed", "chat_id", chatID, "err", err)
		}
	}
	return nil
}

func (l *Loop) considerAndSend(ctx context.Context, chatID int64, now time.Time) error {
	facts, err := l.mem.Facts(ctx, chatID)
	if err != nil {
		return fmt.Errorf("load facts: %w", err)
	}

	reminders, err := db.GetPendingReminders(ctx, l.pool, chatID)
	if err != nil {
		return fmt.Errorf("load reminders: %w", err)
	}
	// Narrow to the next 7 days so the agent isn't overwhelmed by distant items.
	upcoming := filterUpcoming(reminders, now, 7*24*time.Hour)

	turns, err := l.mem.RecentTurns(ctx, chatID, 10)
	if err != nil {
		return fmt.Errorf("load turns: %w", err)
	}

	msg, err := l.agent.Consider(ctx, ConsiderRequest{
		Facts:       facts,
		Reminders:   upcoming,
		RecentTurns: turns,
		Now:         now,
		Loc:         l.loc,
	})
	if err != nil {
		return fmt.Errorf("agent consider: %w", err)
	}
	if msg == "" {
		l.logger.Debug("proactive: agent chose to skip", "chat_id", chatID)
		return nil
	}

	if err := l.msgr.Send(ctx, chatID, msg); err != nil {
		return fmt.Errorf("send proactive message: %w", err)
	}
	l.logger.Info("proactive message sent", "chat_id", chatID)

	if err := l.markSent(ctx, chatID); err != nil {
		// Non-fatal: worst case the user gets another message on the next tick.
		l.logger.Warn("proactive: failed to mark sent", "chat_id", chatID, "err", err)
	}
	return nil
}

// ── DB helpers ────────────────────────────────────────────────────────────────

// eligibleChats returns chat IDs that:
//   - have at least one stored fact (user has engaged with the bot)
//   - have had a conversation turn in the last 7 days (user is still active)
//   - have not received a proactive message within proactiveInterval
const eligibleChatsQuery = `
	SELECT DISTINCT cf.telegram_chat_id
	FROM chat_facts cf
	INNER JOIN chat_turns ct
	       ON cf.telegram_chat_id = ct.telegram_chat_id
	      AND ct.created_at > now() - interval '7 days'
	LEFT JOIN chat_proactive_log pl
	       ON cf.telegram_chat_id = pl.telegram_chat_id
	WHERE pl.last_sent_at IS NULL
	   OR pl.last_sent_at < now() - ($1 * interval '1 second')
`

func (l *Loop) eligibleChats(ctx context.Context) ([]int64, error) {
	rows, err := l.pool.Query(ctx, eligibleChatsQuery, int64(l.proactiveInterval.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("query eligible chats: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan chat id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const markSentQuery = `
	INSERT INTO chat_proactive_log (telegram_chat_id, last_sent_at)
	VALUES ($1, now())
	ON CONFLICT (telegram_chat_id) DO UPDATE
		SET last_sent_at = EXCLUDED.last_sent_at
`

func (l *Loop) markSent(ctx context.Context, chatID int64) error {
	_, err := l.pool.Exec(ctx, markSentQuery, chatID)
	return err
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func filterUpcoming(reminders []db.Reminder, now time.Time, window time.Duration) []db.Reminder {
	cutoff := now.Add(window)
	var out []db.Reminder
	for _, r := range reminders {
		if r.ScheduledAt.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out
}

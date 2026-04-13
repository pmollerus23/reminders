// Package summarize provides a background loop that compresses old chat turns
// into a rolling summary, keeping only the most recent N turns verbatim.
package summarize

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/pmollerus23/reminders/internal/memory"
)

// Summarizer condenses a list of turns (plus any existing summary text) into
// a new summary string. Implementations will typically call an LLM.
// Defined here (consumer side) so the memory package stays pure storage.
type Summarizer interface {
	Summarize(ctx context.Context, existing string, newTurns []memory.Turn) (string, error)
}

// Loop runs on a timer and summarizes chats whose turn count exceeds
// verbatimWindow. It mirrors the shape of scheduler.Run exactly:
// ticker-based, log-and-skip per chat, returns nil on context cancellation.
type Loop struct {
	store          memory.Store
	summarizer     Summarizer
	logger         *slog.Logger
	interval       time.Duration
	verbatimWindow int // keep this many recent turns verbatim; compress older ones
}

// NewLoop constructs a Loop ready to Run.
func NewLoop(
	store memory.Store,
	s Summarizer,
	verbatimWindow int,
	interval time.Duration,
	logger *slog.Logger,
) *Loop {
	return &Loop{
		store:          store,
		summarizer:     s,
		logger:         logger,
		interval:       interval,
		verbatimWindow: verbatimWindow,
	}
}

// Run ticks on l.interval until ctx is cancelled.
// One bad chat does not stop the loop — errors are logged and skipped,
// matching how scheduler.claimAndProcess handles per-reminder failures.
func (l *Loop) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	l.logger.Info("summarize loop started",
		"interval", l.interval,
		"verbatim_window", l.verbatimWindow,
	)

	for {
		select {
		case <-ctx.Done():
			l.logger.Info("summarize loop shutting down")
			return nil
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

// tick is the body of one loop iteration. It is unexported but package-visible
// so tests in the same package can call it directly without timing gymnastics.
func (l *Loop) tick(ctx context.Context) {
	chatIDs, err := l.store.ChatsNeedingSummary(ctx, l.verbatimWindow)
	if err != nil {
		l.logger.Error("summarize: list chats needing summary", "err", err)
		return
	}

	for _, chatID := range chatIDs {
		if err := l.summarizeChat(ctx, chatID); err != nil {
			// Log and continue. One failing chat should not block the others.
			l.logger.Error("summarize: chat failed", "chat_id", chatID, "err", err)
		}
	}
}

// summarizeChat loads turns for chatID, calls the Summarizer, upserts the
// result, and deletes the folded turns.
//
// TODO: make atomic — UpsertSummary and DeleteTurnsBefore are two separate
// writes. A crash between them results in at-least-once summarization: the
// next tick re-folds already-summarized turns, producing a slightly redundant
// summary. This is acceptable because the Summarizer is expected to be
// idempotent (folding the same turns twice produces a coherent summary).
// Making this atomic would require exposing transaction control through
// memory.Store, which adds complexity for a rare failure mode.
func (l *Loop) summarizeChat(ctx context.Context, chatID int64) error {
	// Fetch more turns than we'll ever need in one chat so we get them all.
	// We can't know the exact count without an extra query; 10k is a safe ceiling.
	allTurns, err := l.store.RecentTurns(ctx, chatID, 10_000)
	if err != nil {
		return fmt.Errorf("load turns: %w", err)
	}

	if len(allTurns) <= l.verbatimWindow {
		// Another tick may have already summarized; nothing to do.
		return nil
	}

	// Split: compress the oldest turns, keep the newest verbatimWindow verbatim.
	cutoffIdx := len(allTurns) - l.verbatimWindow
	toSummarize := allTurns[:cutoffIdx]
	// cutoffTime is the CreatedAt of the newest turn being folded. We use it
	// both as SummarizedThrough and as the DeleteTurnsBefore cutoff.
	cutoffTime := toSummarize[len(toSummarize)-1].CreatedAt

	existing, err := l.store.GetSummary(ctx, chatID)
	if err != nil {
		return fmt.Errorf("load summary: %w", err)
	}
	existingText := ""
	if existing != nil {
		existingText = existing.Text
	}

	newText, err := l.summarizer.Summarize(ctx, existingText, toSummarize)
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}

	if err := l.store.UpsertSummary(ctx, chatID, memory.Summary{
		Text:              newText,
		SummarizedThrough: cutoffTime,
	}); err != nil {
		return fmt.Errorf("upsert summary: %w", err)
	}

	if err := l.store.DeleteTurnsBefore(ctx, chatID, cutoffTime); err != nil {
		return fmt.Errorf("delete turns before cutoff: %w", err)
	}

	l.logger.Info("summarize: chat summarized",
		"chat_id", chatID,
		"turns_folded", len(toSummarize),
		"cutoff", cutoffTime,
	)
	return nil
}

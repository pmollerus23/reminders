package intent

import (
	"context"
	"time"

	"github.com/pmollerus23/reminders/internal/promptctx"
)

type stubDispatcher struct{}

// NewStubDispatcher returns a Dispatcher that always rejects with a clear
// message. Used when PARSER=regex, since the unified dispatcher requires an
// LLM. Follows the same stub pattern as fact.NewStubParser.
func NewStubDispatcher() Dispatcher { return &stubDispatcher{} }

func (s *stubDispatcher) Dispatch(_ context.Context, _ promptctx.Prompt, _ time.Time, _ *time.Location) (Action, error) {
	return Action{}, &DispatchError{
		UserMessage: "Natural language mode requires PARSER=claude.",
	}
}

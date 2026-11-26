package dispatcher

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// GitHub #914: Slack reports a turn deadline on a context that is still
// live (its own 10-minute dispatch bound), so FailureNotice must read the
// deadline from the error, not only from ctx; before, it said "a model call
// timed out".
func TestFailureNotice_TurnDeadlineOnLiveContext_914(t *testing.T) {
	err := fmt.Errorf("%w (limit 5m0s): %w", conversation.ErrTurnDeadline, context.DeadlineExceeded)
	got := FailureNotice(context.Background(), err, false)
	if !strings.Contains(got, "this chat turn ran out its time limit") {
		t.Fatalf("notice = %q, want the turn's time limit", got)
	}
}

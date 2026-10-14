package email

import (
	"context"
	"fmt"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// audit 2026-10-12: GitHub #142 §S3 #5 (outcome-quality LLD §S5 #5, design
// review ace9 #1). A turn that ran out its own turn bound took longer than a
// turn allows; retrying it is a deterministic re-timeout that holds this
// mailbox for up to maxDeliveryAttemptsPerMessage × the bound and re-runs the
// turn's tool side effects each time. It is marked seen on the first failure.
func TestHandleRawMessage_TurnDeadlineIsNotRetried(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP()
	cfg.IMAPClient = imap
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.recv = &captureReceiver{err: fmt.Errorf("%w (limit 10m0s): %w", conversation.ErrTurnDeadline, context.DeadlineExceeded)}
	raw := RawMessage{
		UID:  "slow",
		Body: sampleEmail("a@b", "alice@external.test", "Hello vornik", "Please do a long thing"),
	}

	ch.handleRawMessage(context.Background(), raw)

	seen := imap.snapshotMarkSeen()
	if len(seen) != 1 || seen[0] != "slow" {
		t.Fatalf("MarkSeen = %v after a turn-deadline failure, want [slow]: the turn would be retried %d times",
			seen, maxDeliveryAttemptsPerMessage)
	}
}

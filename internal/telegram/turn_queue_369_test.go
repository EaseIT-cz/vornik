package telegram

// GitHub #369 §S3 #11 (outcome-quality design, Amendment 2026-11-18 (#369),
// item 3): at most 3 Telegram turns wait per chat. Found at: review (the #142
// audit); observed at: production (a chat). Test at: unit; Lower because the
// bound is in-process state the handler drives directly.

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestTurnQueue369_AtMostThreeWaitPerChat(t *testing.T) {
	b := &Bot{metrics: NewMetrics(prometheus.NewRegistry())}
	for i := 0; i < maxWaitingTurns; i++ {
		if !b.admitReceiverTurn(7) {
			t.Fatalf("turn %d refused, want %d admitted", i+1, maxWaitingTurns)
		}
	}
	if b.admitReceiverTurn(7) {
		t.Fatal("a fourth waiting turn was admitted")
	}
	if !b.admitReceiverTurn(8) {
		t.Fatal("another chat's turn was refused: the bound is per chat")
	}
	if got := testutil.ToFloat64(b.metrics.TurnsRefused); got != 1 {
		t.Fatalf("turns refused = %v, want 1", got)
	}
	// A turn that takes the lock stops waiting, which frees a slot.
	b.receiverTurnStarted(7)
	if !b.admitReceiverTurn(7) {
		t.Fatal("a slot freed by a started turn was not reused")
	}
}

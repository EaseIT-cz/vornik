package testwait

import (
	"testing"
	"time"
)

type fixedDeadline struct {
	at time.Time
	ok bool
}

func (f fixedDeadline) Deadline() (time.Time, bool) { return f.at, f.ok }

// GitHub #423: the wait must track the binary's budget, not a constant.
func TestForOutcome_LeavesATenthInReserve(t *testing.T) {
	now := time.Now()
	got := forOutcome(fixedDeadline{at: now.Add(100 * time.Second), ok: true}, now)
	if got != 90*time.Second {
		t.Errorf("100 s left: wait = %s, want 90s", got)
	}
}

func TestForOutcome_ReserveIsAtLeastOneSecond(t *testing.T) {
	now := time.Now()
	got := forOutcome(fixedDeadline{at: now.Add(5 * time.Second), ok: true}, now)
	if got != 4*time.Second {
		t.Errorf("5 s left: wait = %s, want 4s", got)
	}
}

func TestForOutcome_ReserveIsAtMostThirtySeconds(t *testing.T) {
	now := time.Now()
	got := forOutcome(fixedDeadline{at: now.Add(10 * time.Minute), ok: true}, now)
	if got != 10*time.Minute-30*time.Second {
		t.Errorf("10 min left: wait = %s, want 9m30s", got)
	}
}

func TestForOutcome_NeverBelowAMillisecond(t *testing.T) {
	now := time.Now()
	for _, left := range []time.Duration{500 * time.Millisecond, 0, -time.Second} {
		if got := forOutcome(fixedDeadline{at: now.Add(left), ok: true}, now); got != time.Millisecond {
			t.Errorf("%s left: wait = %s, want 1ms", left, got)
		}
	}
}

func TestForOutcome_NoDeadline(t *testing.T) {
	if got := ForOutcome(fixedDeadline{}); got != NoDeadline {
		t.Errorf("no deadline: wait = %s, want %s", got, NoDeadline)
	}
}

// The real *testing.T satisfies Deadliner, and under go test's default
// timeout the wait is far longer than the 10 s the #423 test used.
func TestForOutcome_RealTest(t *testing.T) {
	got := ForOutcome(t)
	if _, ok := t.Deadline(); ok && got <= 0 {
		t.Errorf("wait = %s, want positive", got)
	}
}

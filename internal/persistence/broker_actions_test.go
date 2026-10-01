package persistence

import (
	"testing"
	"time"
)

// Design 2026-09-29 §5.4: executing/unknown rows are stuck after 15 minutes,
// approved rows after 5, aged from the approval.
func TestBrokerActionIsStuck(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	cases := []struct {
		name    string
		status  string
		decided *time.Time
		want    bool
	}{
		{"approved 6m", BrokerActionApproved, at(6 * time.Minute), true},
		{"approved 4m", BrokerActionApproved, at(4 * time.Minute), false},
		{"approved without decided_at", BrokerActionApproved, nil, false},
		{"executing 16m", BrokerActionExecuting, at(16 * time.Minute), true},
		{"executing 14m", BrokerActionExecuting, at(14 * time.Minute), false},
		{"unknown exactly 15m", BrokerActionUnknown, at(15 * time.Minute), true},
		{"pending 2h", BrokerActionPending, at(2 * time.Hour), false},
		{"executed 2h", BrokerActionExecuted, at(2 * time.Hour), false},
	}
	for _, c := range cases {
		a := &BrokerAction{Status: c.status, DecidedAt: c.decided, CreatedAt: now.Add(-3 * time.Hour)}
		if got := BrokerActionIsStuck(a, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if BrokerActionIsStuck(nil, now) {
		t.Error("nil row is stuck")
	}
	// No decided_at: an executing row falls back to created_at, as the
	// store's COALESCE does.
	if !BrokerActionIsStuck(&BrokerAction{Status: BrokerActionExecuting, CreatedAt: now.Add(-time.Hour)}, now) {
		t.Error("executing without decided_at must age from created_at")
	}
}

// review-20260930-1334 F1: executing and unknown rows age from executed_at
// (the claim, then the terminal write), not from the approval.
func TestBrokerActionIsStuck_AgesFromTheClaim(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	approved := now.Add(-time.Hour)
	claimed := now.Add(-time.Minute)
	a := &BrokerAction{Status: BrokerActionExecuting, DecidedAt: &approved, ExecutedAt: &claimed}
	if BrokerActionIsStuck(a, now) {
		t.Fatal("claimed a minute ago, approved an hour ago: not stuck")
	}
	if got := BrokerActionAgeSince(a); !got.Equal(claimed) {
		t.Fatalf("age since = %v, want the claim", got)
	}
}

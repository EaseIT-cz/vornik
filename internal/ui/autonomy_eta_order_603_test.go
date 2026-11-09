package ui

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The autonomy next-eval list moved from the overview to /ui/projects (GitHub
// #603) and its hand-written insertion sort became sort.SliceStable: pin the
// order it had. Soonest first, a project with no evaluation last, ties in
// registry order.
func TestAutonomyETAs_603_Order(t *testing.T) {
	now := time.Now().UTC()
	at := func(ago time.Duration) *persistence.AutonomyEvaluation {
		return &persistence.AutonomyEvaluation{CreatedAt: now.Add(-ago), Outcome: "noop"}
	}
	etas := []AutonomyProjectETA{
		autonomyETA("never", "10m", nil, now),
		autonomyETA("late", "10m", at(time.Minute), now),   // next in 9m
		autonomyETA("soon", "10m", at(9*time.Minute), now), // next in 1m
		autonomyETA("never2", "", nil, now),
	}
	sortAutonomyETAs(etas)
	got := []string{etas[0].ProjectID, etas[1].ProjectID, etas[2].ProjectID, etas[3].ProjectID}
	want := []string{"soon", "late", "never", "never2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
	if etas[3].Interval != 5*time.Minute || etas[2].EtaLabel != "≤ 10m 0s ago" {
		t.Fatalf("defaults: interval %v, label %q", etas[3].Interval, etas[2].EtaLabel)
	}
}

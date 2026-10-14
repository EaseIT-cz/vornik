package service

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// audit 2026-10-12: GitHub #142 §S3 #16 (outcome-quality LLD §S5 #16). The
// "model circuit opened" alert could not say why the circuit opened, so the
// operator was told to "investigate the provider" whether the model was
// degraded (a share of calls failing) or had stopped answering altogether.
func TestModelCircuitAlert_NamesTheTripReasonAndItsRemedy(t *testing.T) {
	title, body, ok := modelCircuitAlert("route-a", "m1", "open", chat.TripReasonConsecutive)
	if !ok || !strings.Contains(title, "opened") {
		t.Fatalf("open edge: ok=%v title=%q", ok, title)
	}
	for _, want := range []string{"m1", "route-a", chat.TripReasonConsecutive, chat.TripReasonRemedy(chat.TripReasonConsecutive)} {
		if !strings.Contains(body, want) {
			t.Errorf("alert body %q lacks %q", body, want)
		}
	}
}

func TestModelCircuitAlert_OpenWithoutAReasonKeepsTheOldBody(t *testing.T) {
	_, body, ok := modelCircuitAlert("r", "m", "open", "")
	if !ok || strings.Contains(body, "tripped:") || strings.Contains(body, "()") {
		t.Errorf("open edge with no reason: ok=%v body=%q", ok, body)
	}
}

func TestModelCircuitAlert_RecoveryAndHalfOpen(t *testing.T) {
	if title, _, ok := modelCircuitAlert("r", "m", "closed", ""); !ok || !strings.Contains(title, "recovered") {
		t.Errorf("closed edge: ok=%v title=%q", ok, title)
	}
	if _, _, ok := modelCircuitAlert("r", "m", "half_open", ""); ok {
		t.Error("half_open must not alert (unchanged)")
	}
}

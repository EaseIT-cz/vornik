package api

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// audit 2026-10-12: GitHub #142 §S3 #16 (outcome-quality LLD §S5 #16). Both
// doctor circuit checks said a circuit was OPEN and never why, though the
// breaker knows (rate / consecutive / probe_failed) and the three need
// different checks.
func TestDoctorCircuits_OpenLineNamesTheTripReason(t *testing.T) {
	openAt := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	snaps := func() []chat.ModelHealthSnapshot {
		return []chat.ModelHealthSnapshot{
			{Route: "agent", Model: "m1", State: "open", OpenSince: openAt, TripReason: chat.TripReasonConsecutive},
			{Route: "agent", Model: "m2", State: "open", OpenSince: openAt},
		}
	}
	h := &DoctorHandlers{modelCircuits: snaps, agentCircuits: snaps}
	for name, got := range map[string]DoctorCheck{
		"model_circuits":       h.checkModelCircuits(),
		"agent_model_circuits": h.checkAgentModelCircuits(),
	} {
		var withReason, without string
		for _, it := range got.Items {
			switch {
			case strings.Contains(it, "/m1 "):
				withReason = it
			case strings.Contains(it, "/m2 "):
				without = it
			}
		}
		want := "(tripped: consecutive — " + chat.TripReasonRemedy(chat.TripReasonConsecutive) + ")"
		if !strings.Contains(withReason, want) {
			t.Errorf("%s: open line %q lacks %q", name, withReason, want)
		}
		if strings.Contains(without, "tripped:") {
			t.Errorf("%s: open line with no recorded reason grew a suffix: %q", name, without)
		}
	}
}

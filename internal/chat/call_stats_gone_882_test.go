package chat

// GitHub #882, retired-model design §2: a 410 from a provider means the model
// is retired. CallStats records when and why, even at its entry cap (up to
// 32 extra keys), and counts the ones it still has to drop.

import (
	"errors"
	"fmt"
	"testing"
)

func TestCallStatsGone_882(t *testing.T) {
	s := &CallStats{}
	gone := &GatewayError{Status: 410, Message: "minimax-m2.7 was retired on November 9, 2026."}
	s.Record("retired", "chat.external", gone)
	s.Record("wrapped", "chat.external", fmt.Errorf("call: %w", gone))
	s.Record("notfound", "chat.external", &GatewayError{Status: 404, Message: "no such route"})
	s.Record("broken", "chat.external", &GatewayError{Status: 500, Message: "boom"})
	s.Record("plain", "chat.external", errors.New("deadline exceeded"))

	got := map[string]CallStat{}
	for _, c := range s.Snapshot() {
		got[c.Model] = c
	}
	for _, m := range []string{"retired", "wrapped"} {
		if got[m].GoneAt.IsZero() || got[m].GoneMessage != gone.Message {
			t.Errorf("%s: GoneAt %v, GoneMessage %q; a 410 must be recorded as gone", m, got[m].GoneAt, got[m].GoneMessage)
		}
	}
	for _, m := range []string{"notfound", "broken", "plain"} {
		if !got[m].GoneAt.IsZero() || got[m].GoneMessage != "" {
			t.Errorf("%s recorded as gone; only a 410 is", m)
		}
	}
}

func TestCallStatsGoneAtCap_882(t *testing.T) {
	s := &CallStats{}
	for i := 0; i < callStatsMaxEntries; i++ {
		s.Record(fmt.Sprintf("m%d", i), "site", nil)
	}
	s.Record("new-ok", "site", nil)
	gone := &GatewayError{Status: 410, Message: "retired"}
	for i := 0; i < callStatsMaxGoneExtra+3; i++ {
		s.Record(fmt.Sprintf("gone%d", i), "site", gone)
	}
	recorded, sawNewOK := 0, false
	for _, c := range s.Snapshot() {
		if !c.GoneAt.IsZero() {
			recorded++
		}
		sawNewOK = sawNewOK || c.Model == "new-ok"
	}
	if sawNewOK {
		t.Error("at the cap a non-410 new key must still be dropped")
	}
	if recorded != callStatsMaxGoneExtra {
		t.Errorf("recorded %d gone keys past the cap, want %d", recorded, callStatsMaxGoneExtra)
	}
	if d := s.DroppedGone(); d != 3 {
		t.Errorf("DroppedGone = %d, want 3", d)
	}
}

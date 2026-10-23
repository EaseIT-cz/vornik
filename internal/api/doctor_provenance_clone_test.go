package api

import (
	"strings"
	"testing"
	"time"
)

// memory_provenance_clone (GitHub #499; design 22, part 8g): the doctor
// reports each configured daemon-only clone's state and last fetch, and how
// many it examined.
func TestCheckProvenanceClone(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		src      func() []ProvenanceCloneStatus
		status   string
		mentions []string
	}{
		{"not wired", nil, "SKIPPED", []string{"not wired"}},
		{"none configured", func() []ProvenanceCloneStatus { return nil }, "OK", []string{"0 provenance clones", "automatic clone"}},
		{"fetched", func() []ProvenanceCloneStatus {
			return []ProvenanceCloneStatus{{ProjectID: "p", Usable: true, Fetched: true, FetchOutcome: "ok", FetchedAt: at}}
		}, "OK", []string{"1 provenance clone examined", "p: fetched 2026-10-22T09:00:00Z"}},
		{"not fetched yet", func() []ProvenanceCloneStatus {
			return []ProvenanceCloneStatus{{ProjectID: "p", Usable: true}}
		}, "OK", []string{"not fetched yet"}},
		{"fetch failed", func() []ProvenanceCloneStatus {
			return []ProvenanceCloneStatus{
				{ProjectID: "a", Usable: true, Fetched: true, FetchOutcome: "ok", FetchedAt: at},
				{ProjectID: "b", Usable: true, Fetched: true, FetchOutcome: "timeout", FetchError: "killed", FetchedAt: at},
			}
		}, "WARNING", []string{"2 provenance clones examined", "b: last fetch timeout", "killed", "unverified"}},
		{"refused", func() []ProvenanceCloneStatus {
			return []ProvenanceCloneStatus{{ProjectID: "c"}}
		}, "WARNING", []string{"c: clone refused"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			if c.src != nil {
				h.SetProvenanceClones(c.src)
			}
			got := h.checkProvenanceClone()
			if got.Name != "memory_provenance_clone" || got.Status != c.status {
				t.Fatalf("got %s %s (%s), want %s", got.Name, got.Status, got.Message, c.status)
			}
			for _, m := range c.mentions {
				if !strings.Contains(got.Message, m) {
					t.Errorf("message %q does not mention %q", got.Message, m)
				}
			}
		})
	}
}

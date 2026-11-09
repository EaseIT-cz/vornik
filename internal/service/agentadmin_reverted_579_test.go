package service

import (
	"errors"
	"fmt"
	"testing"

	"vornik.io/vornik/internal/controlplane"
)

// GitHub #579 (agent-administered design part 22, §22.2.16a). Found at:
// production (2026-10-28): a reload-rejected inert change the engine reverted
// and verified left ns:hermes busy, so two phone-approved widenings failed at
// BeginConfig about 200 times each. Regression test at: unit, the decision
// applyAndVerify makes. Lower because: the production chain needed a
// daemon-wide reload failure; the engine half is pinned at integration level
// in controlplane/apply_journal_reverted_579_test.go.
func TestApplyRefusalUnpublished_579(t *testing.T) {
	reverted := fmt.Errorf("apply reload rejected (reverted): %w (%w)", errors.New("bad yaml"), controlplane.ErrReverted)
	cases := []struct {
		name    string
		err     error
		resumes bool
		want    bool
	}{
		{"verified revert", reverted, false, true},
		{"verified revert of a resumed row", reverted, true, true},
		{"pre-write sentinel", controlplane.ErrBusy, false, true},
		{"pre-write sentinel on a resume", controlplane.ErrBusy, true, false},
		{"a drift: no verified restore", controlplane.ErrJournalDrift, false, false},
		{"a plain failure", errors.New("disk on fire"), false, false},
	}
	for _, c := range cases {
		if got := applyRefusalUnpublished(c.err, c.resumes); got != c.want {
			t.Errorf("%s: unpublished = %v, want %v", c.name, got, c.want)
		}
	}
}

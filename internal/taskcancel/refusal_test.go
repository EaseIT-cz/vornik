package taskcancel

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestRefusalReason — audit 2026-10-12 (GitHub #326): the companion cancel
// answered "task was already terminal; cancel is a no-op" for every refused
// write, including a task parked in WAITING_FOR_CHILDREN, which is not
// terminal: the narrow from-set simply excludes it. A refused cancel is
// phrased from the row's current status and says why (05-scheduler §4.11,
// "Amended 2026-10-12").
func TestRefusalReason(t *testing.T) {
	cases := []struct {
		name    string
		status  persistence.TaskStatus
		from    []persistence.TaskStatus
		want    []string
		notWant []string
	}{
		{"terminal completed", persistence.TaskStatusCompleted, nil,
			[]string{"already COMPLETED", "nothing to cancel"}, nil},
		{"terminal closed", persistence.TaskStatusClosed, nil,
			[]string{"already CLOSED"}, nil},
		{"terminal cancelled", persistence.TaskStatusCancelled, nil,
			[]string{"already CANCELLED"}, nil},
		{"terminal failed", persistence.TaskStatusFailed, nil,
			[]string{"already FAILED"}, nil},
		{"parked waiting for children, narrow set", persistence.TaskStatusWaitingForChildren, nil,
			[]string{"WAITING_FOR_CHILDREN", "does not act on", "PENDING, QUEUED, LEASED, RUNNING or PAUSED", "left unchanged"},
			[]string{"terminal", "already"}},
		{"parked awaiting approval, narrow set", persistence.TaskStatusAwaitingApproval, nil,
			[]string{"AWAITING_APPROVAL", "does not act on"}, []string{"terminal"}},
		{"in the narrow set: moved twice", persistence.TaskStatusRunning, nil,
			[]string{"changed while cancelling", "now RUNNING", "try again"}, []string{"terminal"}},
		{"explicit set excludes the status", persistence.TaskStatusPaused,
			[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval},
			[]string{"PAUSED", "does not act on", "AWAITING_APPROVAL tasks"}, nil},
		{"explicit set includes the status", persistence.TaskStatusAwaitingInput,
			persistence.CancellableTaskStatuses(),
			[]string{"changed while cancelling", "now AWAITING_INPUT"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RefusalReason(tc.status, tc.from)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("RefusalReason(%s) = %q, missing %q", tc.status, got, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("RefusalReason(%s) = %q, must not contain %q", tc.status, got, w)
				}
			}
		})
	}
}

// TestRefusalReason_AwaitingApproval — GitHub #326, operator decision
// 2026-10-13: the client surfaces refuse AWAITING_APPROVAL on purpose; the
// reply names the real status and says an approval is answered, not
// cancelled.
func TestRefusalReason_AwaitingApproval(t *testing.T) {
	got := RefusalReason(persistence.TaskStatusAwaitingApproval, persistence.ClientCancellableTaskStatuses())
	for _, want := range []string{"AWAITING_APPROVAL", "does not act on", "approval is answered", "approve or reject", "left unchanged"} {
		if !strings.Contains(got, want) {
			t.Errorf("RefusalReason = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "terminal") || strings.Contains(got, "already") {
		t.Errorf("RefusalReason = %q must not call a parked task finished", got)
	}
}

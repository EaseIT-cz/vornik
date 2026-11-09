package persistence

import (
	"strings"
	"testing"
)

// GitHub #490 (05-scheduler §4.11 "Amended 2026-11-07"): the one builder of a
// closed approval's reason.
func TestApprovalClosedReason_490(t *testing.T) {
	why := "approval timed out"
	long := strings.Repeat("é", 400) // 800 bytes, two per rune
	cases := []struct {
		name string
		root *Task
		want string
	}{
		{"cancelled with a reason", &Task{Status: TaskStatusCancelled, LastError: &why},
			"approval closed: task r1 above it was cancelled: approval timed out"},
		{"cancelled, no reason", &Task{Status: TaskStatusCancelled},
			"approval closed: task r1 above it was cancelled"},
		{"retried", &Task{Status: TaskStatusQueued, LastError: &why},
			"approval closed: task r1 above it was retried"},
		{"unreadable", nil, "approval closed: task r1 above it was cancelled"},
	}
	for _, c := range cases {
		if got := ApprovalClosedReason("r1", c.root); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	got := ApprovalClosedReason("r1", &Task{Status: TaskStatusCancelled, LastError: &long})
	tail := strings.TrimPrefix(got, "approval closed: task r1 above it was cancelled: ")
	if len(tail) > 300 || !strings.HasPrefix(long, tail) || len(tail)%2 != 0 {
		t.Errorf("the root's reason must be cut at 300 bytes on a rune boundary: %d bytes", len(tail))
	}
	// The prefix is matched with LIKE '<prefix>%' in both drivers.
	if strings.ContainsAny(ApprovalClosedPrefix, "%_") || !strings.HasPrefix(ApprovalClosedReason("r1", nil), ApprovalClosedPrefix) {
		t.Errorf("ApprovalClosedPrefix %q must hold no LIKE wildcard and start every reason", ApprovalClosedPrefix)
	}
}

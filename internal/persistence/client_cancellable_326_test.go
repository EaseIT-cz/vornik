package persistence

import (
	"slices"
	"testing"
)

// TestClientCancellableTaskStatuses — GitHub #326, operator decision
// 2026-10-13: the companion, REST and Telegram cancels act on every status
// the UI and chat cancel, except AWAITING_APPROVAL (an approval is answered,
// not cancelled from those surfaces). Derived from CancellableTaskStatuses,
// so the two cannot drift (05-scheduler §4.11 "Amended 2026-10-13").
func TestClientCancellableTaskStatuses(t *testing.T) {
	got := ClientCancellableTaskStatuses()
	if slices.Contains(got, TaskStatusAwaitingApproval) {
		t.Fatalf("client set must exclude AWAITING_APPROVAL: %v", got)
	}
	want := slices.DeleteFunc(CancellableTaskStatuses(), func(s TaskStatus) bool {
		return s == TaskStatusAwaitingApproval
	})
	if !slices.Equal(got, want) {
		t.Fatalf("client set = %v, want the full cancellable set minus AWAITING_APPROVAL %v", got, want)
	}
	for _, s := range []TaskStatus{TaskStatusWaitingForChildren, TaskStatusAwaitingInput, TaskStatusAwaitingExternal} {
		if !slices.Contains(got, s) {
			t.Errorf("client set must include %s", s)
		}
	}
	got[0] = TaskStatusClosed
	if ClientCancellableTaskStatuses()[0] == TaskStatusClosed {
		t.Fatal("ClientCancellableTaskStatuses must return a fresh slice")
	}
}

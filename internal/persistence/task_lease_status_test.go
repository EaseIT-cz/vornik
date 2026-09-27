package persistence

import "testing"

// Scheduler design §4.9 (review 6fdb minor): exactly CANCELLED and CLOSED hold
// no lease. A new status joining the set — or FAILED/COMPLETED, which would
// drop the scheduler's retry handoff — must fail here, not silently.
func TestTaskStatusHoldsNoLease_IsExactlyCancelledAndClosed(t *testing.T) {
	all := []TaskStatus{
		TaskStatusPending, TaskStatusQueued, TaskStatusLeased, TaskStatusRunning,
		TaskStatusWaitingForChildren, TaskStatusCompleted, TaskStatusFailed,
		TaskStatusCancelled, TaskStatusAwaitingInput, TaskStatusAwaitingExternal,
		TaskStatusPaused, TaskStatusClosed, TaskStatusAwaitingApproval,
	}
	for _, s := range all {
		want := s == TaskStatusCancelled || s == TaskStatusClosed
		if got := TaskStatusHoldsNoLease(s); got != want {
			t.Errorf("TaskStatusHoldsNoLease(%s) = %v, want %v", s, got, want)
		}
	}
}

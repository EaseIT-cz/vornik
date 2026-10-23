package ui

import (
	"slices"

	"vornik.io/vornik/internal/persistence"
)

// TaskStatusControls is which status controls the task detail page offers.
// ONE function decides it (taskStatusControls) and ONE template partial
// ("taskStatusControls" in task_detail.html) renders it, for both the desktop
// header row and the phone action bar, so the two layouts cannot offer
// different controls again (GitHub #375; 13-control-plane-web-ui.md "Update —
// 2026-10-16: task-detail status controls").
type TaskStatusControls struct {
	Pause     bool
	Resume    bool
	Requeue   bool // also gates the "Fallback model" requeue
	WatchLive bool
	Cancel    bool
}

// uiPausableTaskStatuses is the set the UI pause handler (uiPauseTask) moves
// to PAUSED, and so the set the page offers Pause for. AWAITING_APPROVAL is
// not in it: an approval is answered, not paused. Fresh slice per call.
func uiPausableTaskStatuses() []persistence.TaskStatus {
	return []persistence.TaskStatus{
		persistence.TaskStatusPending,
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal,
	}
}

// uiRequeueableTaskStatuses is the set the UI requeue handler (retryOne)
// accepts: the terminal statuses a run can end in, plus PENDING as the
// stuck-waiting safety net (incident 2026-05-16). Fresh slice per call.
func uiRequeueableTaskStatuses() []persistence.TaskStatus {
	return []persistence.TaskStatus{
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusCompleted,
		persistence.TaskStatusPending,
	}
}

// uiLiveTaskStatuses is the set the page links to the live observation page
// for. No handler gates it (TaskLive sends a terminal task to the replay
// page); it is a presentation choice, named once. Fresh slice per call.
func uiLiveTaskStatuses() []persistence.TaskStatus {
	return []persistence.TaskStatus{
		persistence.TaskStatusQueued,
		persistence.TaskStatusPending,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
	}
}

// taskStatusControls decides the controls for a task in status. lifecycle is
// "a task-message repository is wired": the pause and resume handlers answer
// 503 without one, so they are not offered then. Cancel, Requeue and Watch
// live need only the task repository.
func taskStatusControls(status persistence.TaskStatus, lifecycle bool) TaskStatusControls {
	return TaskStatusControls{
		Pause:     lifecycle && slices.Contains(uiPausableTaskStatuses(), status),
		Resume:    lifecycle && status == persistence.TaskStatusPaused,
		Requeue:   slices.Contains(uiRequeueableTaskStatuses(), status),
		WatchLive: slices.Contains(uiLiveTaskStatuses(), status),
		Cancel:    slices.Contains(persistence.CancellableTaskStatuses(), status),
	}
}

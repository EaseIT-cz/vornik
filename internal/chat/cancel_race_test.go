package chat

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// Scheduler design §4.10. The chat cancel read a status, then wrote CANCELLED
// with the bare UpdateStatus — so a task that COMPLETED between the two was
// recorded as cancelled. The race window cannot be hit on demand, so Get
// returns the stale snapshot while the stored row has already moved on.
func staleSnapshotRepo(stale, live persistence.TaskStatus) (*mocks.MockTaskRepository, *persistence.TaskStatus) {
	stored := live
	reads := 0
	return &mocks.MockTaskRepository{
		// The FIRST read is the frozen stale snapshot; any later read (the
		// re-read a refused cancel does) sees the live row — the two diverge
		// by construction (review a3d9 F1).
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			reads++
			if reads == 1 {
				return &persistence.Task{ID: id, ProjectID: "p1", Status: stale}, nil
			}
			return &persistence.Task{ID: id, ProjectID: "p1", Status: stored}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, s persistence.TaskStatus) error {
			stored = s
			return nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			for _, f := range from {
				if f == stored {
					stored = to
					return true, nil
				}
			}
			return false, nil
		},
	}, &stored
}

func TestExecuteCancelTask_DoesNotOverwriteATaskThatCompletedMeanwhile(t *testing.T) {
	repo, stored := staleSnapshotRepo(persistence.TaskStatusRunning, persistence.TaskStatusCompleted)
	res, _ := executeCancelTask(context.Background(), Action{TaskID: "t1"}, repo)
	if *stored != persistence.TaskStatusCompleted {
		t.Fatalf("stored status %s — a COMPLETED task was overwritten by a cancel", *stored)
	}
	if res.Success {
		t.Fatalf("reported success for a cancel that did not happen: %s", res.Message)
	}
	// The refusal names the row's CURRENT status, not the stale snapshot's.
	if !strings.Contains(res.Message, string(persistence.TaskStatusCompleted)) ||
		strings.Contains(res.Message, string(persistence.TaskStatusRunning)) {
		t.Fatalf("refusal message %q should name COMPLETED (live), not RUNNING (snapshot)", res.Message)
	}
}

// CLOSED is an end state; the snapshot check had let it through.
func TestExecuteCancelTask_RefusesAClosedTask(t *testing.T) {
	repo, stored := staleSnapshotRepo(persistence.TaskStatusClosed, persistence.TaskStatusClosed)
	res, _ := executeCancelTask(context.Background(), Action{TaskID: "t1"}, repo)
	if *stored != persistence.TaskStatusClosed || res.Success {
		t.Fatalf("a CLOSED task was cancelled: stored=%s success=%v", *stored, res.Success)
	}
}

func TestExecuteCancelTask_CancelsALiveTask(t *testing.T) {
	repo, stored := staleSnapshotRepo(persistence.TaskStatusRunning, persistence.TaskStatusRunning)
	res, err := executeCancelTask(context.Background(), Action{TaskID: "t1"}, repo)
	if err != nil || !res.Success || *stored != persistence.TaskStatusCancelled {
		t.Fatalf("live cancel: err=%v success=%v stored=%s", err, res.Success, *stored)
	}
}

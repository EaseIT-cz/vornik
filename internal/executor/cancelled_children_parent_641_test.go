package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #641 (incident 2026-11-04 22:29, task_20261104222835_9538337d19fb4bc7):
// the operator cancelled a delegated child; the parent was re-queued, its
// resume guard surfaced "1 delegated subtask(s) completed", and the parent
// ended COMPLETED with success notifications. Design 10-delegation-engine.md
// §4: a failed or cancelled child fails the parent by default.
//
// Found at: production (operator observation in the Telegram chat).
// Regression test at: unit, the executor's own unblock path
// (unblockParentIfChildrenDone) with its repository fakes.
func TestUnblockParent_AllChildrenCancelled_ParentCancelled_641(t *testing.T) {
	e, _, _, _, tr := setup()
	parentID := "parent-641"
	tr.AddTask(&persistence.Task{ID: parentID, ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren, CreatedAt: time.Now()})
	tr.AddTask(&persistence.Task{ID: "child-641", ProjectID: "p1", ParentTaskID: &parentID, Status: persistence.TaskStatusCancelled, CreatedAt: time.Now()})

	rec := &completionRecorder641{}
	e.SetCompletionNotifier(rec)
	e.unblockParentIfChildrenDone(context.Background(), parentID)

	if len(rec.calls) != 1 || rec.calls[0].success || rec.calls[0].taskID != parentID {
		t.Fatalf("notifications = %+v, want one failure notice for the parent (the chat's follow-up waits on it)", rec.calls)
	}
	tr.mu.Lock()
	p := tr.tasks[parentID]
	tr.mu.Unlock()
	if p.Status != persistence.TaskStatusCancelled {
		t.Fatalf("parent status = %s, want CANCELLED: a parent whose every child was cancelled must not resume to COMPLETED", p.Status)
	}
	if p.LastError == nil || !strings.Contains(*p.LastError, "child-641") || !strings.Contains(*p.LastError, "cancelled") {
		t.Fatalf("parent last_error = %v, want it to name the cancelled child", p.LastError)
	}
}

// A parent with one completed and one cancelled child still resumes: the
// completed child's work is real. Its synthesised message must say what
// happened to each child, not "2 delegated subtask(s) completed".
func TestUnblockParent_SomeCompletedSomeCancelled_Resumes_641(t *testing.T) {
	e, _, _, _, tr := setup()
	parentID := "parent-641b"
	tr.AddTask(&persistence.Task{ID: parentID, ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren, CreatedAt: time.Now()})
	tr.AddTask(&persistence.Task{ID: "c-done", ProjectID: "p1", ParentTaskID: &parentID, Status: persistence.TaskStatusCompleted, CreatedAt: time.Now()})
	tr.AddTask(&persistence.Task{ID: "c-cancel", ProjectID: "p1", ParentTaskID: &parentID, Status: persistence.TaskStatusCancelled, CreatedAt: time.Now()})

	e.unblockParentIfChildrenDone(context.Background(), parentID)
	tr.mu.Lock()
	got := tr.tasks[parentID].Status
	tr.mu.Unlock()
	if got != persistence.TaskStatusQueued {
		t.Fatalf("parent status = %s, want QUEUED (one child completed)", got)
	}
}

func TestSynthesizeResumeResult_ReportsEachStatus_641(t *testing.T) {
	e, _, _, _, _ := setup()
	out := e.synthesizeResumeResult(context.Background(), []*persistence.Task{
		{ID: "b", Status: persistence.TaskStatusCancelled},
		{ID: "a", Status: persistence.TaskStatusCompleted},
		{ID: "c", Status: persistence.TaskStatusFailed},
	})
	var r struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	if want := "3 delegated subtask(s): 1 completed, 1 failed, 1 cancelled"; r.Message != want {
		t.Fatalf("message %q, want %q", r.Message, want)
	}
}

type completionCall641 struct {
	taskID  string
	success bool
}

type completionRecorder641 struct{ calls []completionCall641 }

func (r *completionRecorder641) NotifyTaskCompleted(_ context.Context, task *persistence.Task, success bool, _ string) {
	r.calls = append(r.calls, completionCall641{taskID: task.ID, success: success})
}

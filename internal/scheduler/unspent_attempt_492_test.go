package scheduler

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #492 (permanent-failure design §8.8.5). Found at review (the
// 2026-10-21 operator decisions). A retry the open circuit fast-rejected
// spent an attempt although its call never reached a model, so on its last
// attempt the task ended FAILED. Not spent, it is released QUEUED with the
// attempt unchanged, and held as before.

// unspentExec is a hold source that also reports attempts not spent.
type unspentExec struct {
	holdingExec
	unspent map[string]bool
}

func (e *unspentExec) AttemptNotSpent(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.unspent[taskID]
	delete(e.unspent, taskID)
	return v
}

func TestTaskFailed492_UnspentAttemptKeepsTheBudget(t *testing.T) {
	exec := &unspentExec{holdingExec: holdingExec{holds: map[string]time.Time{}}, unspent: map[string]bool{}}
	h := newHoldHarness(t, exec)
	exec.holds["rejected"] = h.clock().Add(30 * time.Second)
	exec.unspent["rejected"] = true
	task := w2LeasedTask("rejected")
	task.Attempt, task.MaxAttempts = 3, 3
	h.repo.AddTask(task)
	h.s.runningCount = 1
	if err := h.s.TaskCompleted("rejected", *task.LeaseID, false, unhealthyNote); err != nil {
		t.Fatal(err)
	}
	got, _ := h.repo.Get(context.Background(), "rejected")
	if got.Status != persistence.TaskStatusQueued {
		t.Fatalf("a reject on the last attempt spent nothing and must retry, got %s", got.Status)
	}
	if got.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3: a reject that reached no model must not spend one", got.Attempt)
	}
	h.s.mu.Lock()
	_, held := h.s.retryHeldUntil["rejected"]
	h.s.mu.Unlock()
	if !held {
		t.Fatal("the unspent retry must still be held until the circuit can admit a call")
	}
}

// The attempt whose call ran (no unspent record) spends as before.
func TestTaskFailed492_SpentAttemptStillEndsTheBudget(t *testing.T) {
	exec := &unspentExec{holdingExec: holdingExec{holds: map[string]time.Time{}}, unspent: map[string]bool{}}
	h := newHoldHarness(t, exec)
	task := w2LeasedTask("ran")
	task.Attempt, task.MaxAttempts = 3, 3
	h.repo.AddTask(task)
	h.s.runningCount = 1
	if err := h.s.TaskCompleted("ran", *task.LeaseID, false, "LLM call failed: 502 bad gateway"); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.repo.Get(context.Background(), "ran"); got.Status != persistence.TaskStatusFailed {
		t.Fatalf("a spent last attempt must fail, got %s", got.Status)
	}
}

// The circuit closed between the reject and the release: no hold, so the
// unspent retry is released at once, attempt still unchanged.
func TestTaskFailed492_UnspentWithoutAHoldRetriesAtOnce(t *testing.T) {
	exec := &unspentExec{holdingExec: holdingExec{holds: map[string]time.Time{}}, unspent: map[string]bool{"closed": true}}
	h := newHoldHarness(t, exec)
	task := w2LeasedTask("closed")
	task.Attempt, task.MaxAttempts = 2, 2
	h.repo.AddTask(task)
	h.s.runningCount = 1
	if err := h.s.TaskCompleted("closed", *task.LeaseID, false, unhealthyNote); err != nil {
		t.Fatal(err)
	}
	got, _ := h.repo.Get(context.Background(), "closed")
	if got.Status != persistence.TaskStatusQueued || got.Attempt != 2 {
		t.Fatalf("status %s attempt %d, want QUEUED at attempt 2", got.Status, got.Attempt)
	}
	h.s.mu.Lock()
	_, held := h.s.retryHeldUntil["closed"]
	h.s.mu.Unlock()
	if held {
		t.Fatal("with no hold recorded the retry must not be held")
	}
}

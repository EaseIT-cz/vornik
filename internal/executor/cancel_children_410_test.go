package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestCancelChildren_EndsEveryUnfinishedChild_410 — GitHub #410 (found at
// code review 2026-10-16, cli-design.md #408 amendment): cancelling a parent
// cascaded only to PENDING/QUEUED/LEASED/RUNNING/WAITING_FOR_CHILDREN
// children, so a PAUSED or AWAITING_* child survived under a CANCELLED parent
// (an approval-parked one could still be approved and run). The cascade now
// ends every status in persistence.CancellableTaskStatuses() and records why;
// finished children are left as they are and the recursion passes through
// them. Replaces TestCancelChildren_LeavesParkedChildrenAlone (#326), which
// pinned the old scope (05-scheduler §4.11 "Amended 2026-10-17").
func TestCancelChildren_EndsEveryUnfinishedChild_410(t *testing.T) {
	parentID := "p410"
	tasks := map[string]*persistence.Task{}
	childrenOf := map[string][]string{}
	add := func(id, parent string, st persistence.TaskStatus) {
		p := parent
		tasks[id] = &persistence.Task{ID: id, ParentTaskID: &p, Status: st}
		childrenOf[parent] = append(childrenOf[parent], id)
	}
	for _, st := range persistence.CancellableTaskStatuses() {
		add("c-"+string(st), parentID, st)
	}
	finished := []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled, persistence.TaskStatusClosed,
	}
	for _, st := range finished {
		add("c-"+string(st), parentID, st)
	}
	// A finished child's own parked child is still reached.
	add("g-paused", "c-"+string(persistence.TaskStatusCompleted), persistence.TaskStatusPaused)

	reasons := map[string]string{}
	repo := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, id string) ([]*persistence.Task, error) {
			var out []*persistence.Task
			for _, cid := range childrenOf[id] {
				out = append(out, tasks[cid])
			}
			return out, nil
		},
		TransitionConditionalFunc: func(_ context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			task := tasks[id]
			for _, s := range from {
				if task.Status != s {
					continue
				}
				task.Status = to
				if opts.LastError != nil {
					reasons[id] = *opts.LastError
				}
				if opts.LastErrorClass == nil || *opts.LastErrorClass != persistence.TaskFailureClassCancelled {
					t.Errorf("%s: LastErrorClass = %v, want CANCELLED", id, opts.LastErrorClass)
				}
				return true, nil
			}
			return false, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.CancelChildren(context.Background(), parentID)

	for _, st := range persistence.CancellableTaskStatuses() {
		id := "c-" + string(st)
		if tasks[id].Status != persistence.TaskStatusCancelled {
			t.Errorf("%s = %s, want CANCELLED (a parent's cancel ends every unfinished child)", id, tasks[id].Status)
		}
		if r := reasons[id]; !strings.Contains(r, parentID) {
			t.Errorf("%s: last_error %q does not name the ancestor %s", id, r, parentID)
		}
	}
	for _, st := range finished {
		if got := tasks["c-"+string(st)].Status; got != st {
			t.Errorf("finished child c-%s = %s, want it left %s", st, got, st)
		}
	}
	if tasks["g-paused"].Status != persistence.TaskStatusCancelled {
		t.Errorf("grandchild under a COMPLETED child = %s, want CANCELLED (the recursion passes finished children)", tasks["g-paused"].Status)
	}
}

// TestCancelBlockedDependents_KeepsItsSet_410 — GitHub #410: the dependents
// sweep (an upstream sibling FAILED) keeps its own five-status set; widening
// it is a separate question (05-scheduler §4.11 "Amended 2026-10-17",
// considered 3). Pinned so the cascade's wider set does not leak into it.
func TestCancelBlockedDependents_KeepsItsSet_410(t *testing.T) {
	dep := &persistence.Task{ID: "dep", Status: persistence.TaskStatusPaused}
	repo := &mocks.MockTaskRepository{
		GetDependentsFunc: func(_ context.Context, id string) ([]*persistence.Task, error) {
			if id == "up" {
				return []*persistence.Task{dep}, nil
			}
			return nil, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			for _, s := range from {
				if dep.Status == s {
					dep.Status = to
					return true, nil
				}
			}
			return false, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.cancelBlockedDependents(context.Background(), "up", "reason", map[string]bool{"up": true})
	if dep.Status != persistence.TaskStatusPaused {
		t.Errorf("PAUSED dependent of a failed sibling = %s, want PAUSED (the dependents sweep keeps its set)", dep.Status)
	}
}

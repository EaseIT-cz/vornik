package watchdog

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #304 (2026-10-08): the approval-timeout sweep (05-scheduler §4.11
// site #12) cancelled a task parked in AWAITING_APPROVAL and did nothing
// after it, so a delegating parent waited in WAITING_FOR_CHILDREN for the
// next stranded-parent sweep, and the task's children ran on. It now goes
// through taskcancel.Cancel with the executor as its teardown.

type recordingTeardown struct{ notified, cascaded, stopped []string }

func (r *recordingTeardown) CancelIfActive(id string) (bool, error) {
	r.stopped = append(r.stopped, id)
	return false, nil
}
func (r *recordingTeardown) NotifyChildTerminal(_ context.Context, id string) {
	r.notified = append(r.notified, id)
}
func (r *recordingTeardown) CancelChildren(_ context.Context, id string) {
	r.cascaded = append(r.cascaded, id)
}

func TestSweepExpiredApprovals_NotifiesParentAndCascades(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tasks := newStubTaskRepo()
	tasks.listResult = []*persistence.Task{
		{ID: "child", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-100 * time.Hour)},
		{ID: "fresh", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-1 * time.Hour)},
	}
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = 96 * time.Hour
	td := &recordingTeardown{}
	w := New(cfg, noRowsExecRepo(), tasks, zerolog.Nop(), nil).WithTaskTeardown(td)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.sweepExpiredApprovals(context.Background())

	assert.Equal(t, persistence.TaskStatusCancelled, tasks.cancelledIDs()["child"])
	assert.Equal(t, []string{"child"}, td.notified, "the parent of the expired child must be unblocked")
	assert.Equal(t, []string{"child"}, td.cascaded, "the expired task's children must be cancelled")
}

// A row that moved on after List read it is not cancelled, and nothing runs.
func TestSweepExpiredApprovals_MovedRowTearsNothingDown(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tasks := newStubTaskRepo()
	tasks.listResult = []*persistence.Task{
		{ID: "approved", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-100 * time.Hour)},
	}
	tasks.live = map[string]persistence.TaskStatus{"approved": persistence.TaskStatusQueued}
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = 96 * time.Hour
	td := &recordingTeardown{}
	w := New(cfg, noRowsExecRepo(), tasks, zerolog.Nop(), nil).WithTaskTeardown(td)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.sweepExpiredApprovals(context.Background())

	assert.Empty(t, td.notified)
	assert.Empty(t, td.cascaded)
	assert.Empty(t, td.stopped)
}

package watchdog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: outcome-quality §S3 #1 (GitHub #142) — "a hung execution
// keeps its slot and its container after the watchdog fails it; the watchdog
// cannot tell stuck from abandoned". The fail action wrote the two rows and
// nothing else: the container ran on and the executor goroutine stayed
// registered. 05-scheduler §4.12 C2/C3: the watchdog names stuck vs abandoned
// from the task's lease and, when its write moved the task row, tears the
// execution down through the cancel primitive's post-write sequence, without
// the child cascade (a stuck failure is a failure, not a cancel).

// stuckFixture is one RUNNING execution, stuck for 45m, whose task is RUNNING
// with a lease expiring leaseIn from now (negative: already expired). The
// execution repository stops listing a row once it was recorded failed, as
// the real one does (it is no longer RUNNING).
func stuckFixture(t *testing.T, now time.Time, leaseIn time.Duration) (*Watchdog, *stubExecRepo, *stubTaskRepo, *recordingTeardown) {
	t.Helper()
	row := runningExec("e1", "t1", "p1", now, 45*time.Minute)
	exec := &stubExecRepo{}
	exec.listFunc = func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		for _, f := range exec.failures() {
			if f.id == row.ID {
				return nil, nil
			}
		}
		return []*persistence.Execution{row}, nil
	}
	tasks := newStubTaskRepo()
	expires := now.Add(leaseIn)
	lease := "lease-t1"
	tasks.setTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning,
		LeaseID: &lease, LeaseExpiresAt: &expires})
	td := &recordingTeardown{}
	cfg := DefaultConfig()
	cfg.Action = ActionFail
	w := New(cfg, exec, tasks, zerolog.Nop(), nil).WithTaskTeardown(td)
	w.ctx = context.Background()
	return w, exec, tasks, td
}

func TestWatchdog_StuckExecutionIsTornDownOnce(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	w, exec, tasks, td := stuckFixture(t, now, 3*time.Minute)
	clock := now
	w.now = func() time.Time { return clock }

	w.scanOnce()

	require.Len(t, exec.failures(), 1)
	msg := exec.failures()[0].message
	assert.Contains(t, msg, "watchdog: execution stuck", "the prefix logs and searches key on")
	assert.Contains(t, msg, "lease is still renewed", "a live lease means the executor is alive but not advancing")
	got, _ := tasks.Get(context.Background(), "t1")
	assert.Equal(t, persistence.TaskStatusFailed, got.Status)
	assert.Equal(t, []string{"t1"}, td.stopped, "the live execution is asked to stop, through the primitive")
	assert.Equal(t, []string{"t1"}, td.notified, "the parent unblock runs")
	assert.Empty(t, td.cascaded, "a stuck failure does not cancel the task's children")

	clock = clock.Add(time.Hour)
	w.scanOnce()
	assert.Equal(t, []string{"t1"}, td.stopped, "the stop is requested exactly once")
	assert.Len(t, exec.failures(), 1)
}

func TestWatchdog_AbandonedExecutionSaysSo(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	w, exec, _, td := stuckFixture(t, now, -10*time.Minute)
	w.now = func() time.Time { return now }

	w.scanOnce()

	require.Len(t, exec.failures(), 1)
	msg := exec.failures()[0].message
	assert.True(t, strings.HasPrefix(msg, "watchdog: execution stuck"), msg)
	assert.Contains(t, msg, "nothing was running it", "an expired lease means no process holds the execution")
	assert.NotContains(t, msg, "lease is still renewed")
	assert.Equal(t, []string{"t1"}, td.stopped, "the live map, not the lease, decides the teardown")
}

func TestWatchdog_TerminalTaskGetsNoTeardown(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	w, exec, tasks, td := stuckFixture(t, now, 3*time.Minute)
	w.now = func() time.Time { return now }
	tasks.setTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusCompleted})

	w.scanOnce()

	require.Len(t, exec.failures(), 1)
	assert.Empty(t, td.stopped, "a task another writer finished owes this scan no teardown")
	assert.Empty(t, td.notified)
}

// Review 3fb6 C-F2: a task §6 recovery already re-queued (or one parked
// behind a leftover RUNNING execution row) is not the watchdog's to fail. Its
// conditional write is refused and no teardown follows; only the execution
// row is recorded. The full-row Update it replaced failed both.
func TestWatchdog_RequeuedTaskIsNotFailed(t *testing.T) {
	for _, st := range []persistence.TaskStatus{persistence.TaskStatusQueued, persistence.TaskStatusAwaitingInput} {
		t.Run(string(st), func(t *testing.T) {
			now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
			w, exec, tasks, td := stuckFixture(t, now, -10*time.Minute)
			w.now = func() time.Time { return now }
			tasks.setTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: st})

			w.scanOnce()

			require.Len(t, exec.failures(), 1, "the execution row is still recorded")
			got, _ := tasks.Get(context.Background(), "t1")
			assert.Equal(t, st, got.Status)
			assert.Empty(t, td.stopped)
			assert.Empty(t, td.notified)
		})
	}
}

package scheduler

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// fakeClock is a settable clock for the scheduler's now seam, so the tests
// below advance time instead of sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// toggleBlocked is a config-apply gate the test flips.
type toggleBlocked struct{ blocked atomic.Bool }

func (g *toggleBlocked) gate(projectID string) (bool, string) {
	if projectID == "digest" && g.blocked.Load() {
		return true, "configuration apply unresolved (journal DRIFT) for digest"
	}
	return false, ""
}

// audit 2026-10-12: outcome-quality §S3 #14 (GitHub #142) — "a config-blocked
// dispatch may strand the task in PENDING". It did: the refusal released the
// lease to PENDING with no reason, the lease query takes only QUEUED, and
// nothing moves a PENDING task with no paused execution back to QUEUED. Once
// the operator resolved the journal the task still never ran. 05-scheduler
// §4.12 C6: it goes back to QUEUED with the reason on the row, and the project
// is backed off so the 1s poll does not lease and refuse it on every tick.
func TestScheduler_ConfigBlockedTaskRunsOnceUnblocked(t *testing.T) {
	exec := &configBlockedExec{}
	repo := NewMockTaskRepository()
	repo.AddTask(&persistence.Task{ID: "task_b", ProjectID: "digest", Status: persistence.TaskStatusQueued})

	gate := &toggleBlocked{}
	gate.blocked.Store(true)
	clock := newFakeClock()
	s := NewWithOptions(repo, &Config{MaxConcurrency: 1, LeaseDurationSeconds: 60, PollInterval: time.Minute},
		WithExecutor(exec), WithConfigBlocked(gate.gate))
	s.now = clock.Now

	s.schedule()
	s.dispatchWg.Wait()

	got, _ := repo.Get(context.Background(), "task_b")
	if got.Status != persistence.TaskStatusQueued {
		t.Fatalf("a config-blocked task must wait QUEUED so it runs once unblocked, got %s", got.Status)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "journal DRIFT") ||
		!strings.Contains(*got.LastError, "config apply journal") {
		t.Fatalf("the row must say why it waits and what resolves it, got last_error=%v", got.LastError)
	}
	if len(exec.executed) != 0 {
		t.Fatalf("a blocked project's task was executed: %v", exec.executed)
	}
	if s.runningCount != 0 {
		t.Fatalf("the refused dispatch must free its slot, runningCount=%d", s.runningCount)
	}

	// Inside the back-off the project is not leased again.
	s.schedule()
	s.dispatchWg.Wait()
	if n := len(repo.leasedTasks); n != 1 {
		t.Fatalf("a blocked project was re-leased inside its back-off: %d leases", n)
	}

	// The operator resolves the journal; after the back-off the task runs.
	gate.blocked.Store(false)
	clock.Advance(configBlockedBackoff + time.Second)
	s.schedule()
	s.dispatchWg.Wait()
	if len(exec.executed) != 1 || exec.executed[0] != "task_b" {
		t.Fatalf("the task must run once the project is unblocked, executed=%v", exec.executed)
	}
}

// liveForeverExec is an executor whose goroutine never returns: IsExecuting
// stays true however long the test runs, as for a goroutine wedged in a call
// with no bound. It counts Cancel calls.
type liveForeverExec struct {
	cancels atomic.Int32
	started chan struct{}
}

func (e *liveForeverExec) ExecuteWithContext(context.Context, string) error {
	close(e.started)
	return nil
}
func (e *liveForeverExec) IsExecuting(string) bool { return true }
func (e *liveForeverExec) Cancel(string) error     { e.cancels.Add(1); return nil }

// audit 2026-10-12: outcome-quality §S3 #1 (GitHub #142) — "a hung execution
// keeps its scheduler slot after the watchdog fails it". The watch loop saw
// the row leave RUNNING, stopped renewing, then waited for IsExecuting to
// turn false with no bound; a goroutine wedged in a call with no bound held
// the global slot until a restart. 05-scheduler §4.12 C5: after
// AbandonedExecutionGrace the dispatch releases its slot, writes no row (the
// writer that moved it owns it) and does not call Cancel (that writer owns the
// teardown, so the container is stopped once, by it).
func TestDispatch_ReleasesSlotOfAGoroutineThatOutlivesItsRow(t *testing.T) {
	repo := NewMockTaskRepository()
	task := w2LeasedTask("task_h")
	task.Status = persistence.TaskStatusRunning
	repo.AddTask(task)

	exec := &liveForeverExec{started: make(chan struct{})}
	clock := newFakeClock()
	s := NewWithOptions(repo, &Config{LeaseDurationSeconds: 60, PollInterval: time.Minute, AbandonedExecutionGrace: 10 * time.Minute},
		WithExecutor(exec))
	s.now = clock.Now
	ticks := make(chan time.Time) // unbuffered: a send is accepted only by a loop still waiting
	s.watchTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	iterated := make(chan struct{}, 1)
	s.watchIterated = func() { iterated <- struct{}{} }
	s.runningCount = 1

	// Another writer (the watchdog) fails the task: the row leaves RUNNING,
	// keeping its lease, so the next renewal misses on the status guard.
	repo.mu.Lock()
	repo.tasks["task_h"].Status = persistence.TaskStatusFailed
	msg := "watchdog: execution stuck"
	repo.tasks["task_h"].LastError = &msg
	repo.mu.Unlock()

	done := make(chan struct{})
	s.dispatchWg.Add(1)
	go func() { s.dispatchTask(task); close(done) }()
	<-exec.started

	// tick runs exactly one loop iteration at the current fake time: the send
	// starts it, and the iterated signal says it reached its next wait. If
	// the loop returns instead, done closes.
	tick := func(what string) {
		t.Helper()
		ticks <- clock.Now()
		select {
		case <-iterated:
		case <-done:
			t.Fatalf("the dispatch returned (%s)", what)
		}
	}
	<-iterated // the first iteration, at the unadvanced clock
	// Past the renewal interval: the renewal misses and the loop sees the row
	// has left RUNNING.
	clock.Advance(60 * time.Second)
	tick("renewal")
	// Inside the grace: the loop must still be waiting for the next tick.
	clock.Advance(9 * time.Minute)
	tick("inside the grace")
	clock.Advance(2 * time.Minute)
	ticks <- clock.Now()
	select {
	case <-done:
	case <-iterated:
		t.Fatal("a goroutine that outlived its row held the slot past the grace")
	}
	if s.runningCount != 0 {
		t.Fatalf("slot not freed: runningCount=%d", s.runningCount)
	}
	if n := exec.cancels.Load(); n != 0 {
		t.Fatalf("the scheduler must not tear down again (the row's writer owns it), Cancel called %d times", n)
	}
	got, _ := repo.Get(context.Background(), "task_h")
	if got.Status != persistence.TaskStatusFailed || got.LastError == nil || *got.LastError != msg {
		t.Fatalf("the row's writer owns it; got status=%s last_error=%v", got.Status, got.LastError)
	}
}

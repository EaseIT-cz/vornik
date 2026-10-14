package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/watchdog"
)

// wedgedExecutor stands in for an executor whose goroutine is wedged in a call
// with no bound: it is live from the moment it is asked to run, and stays live
// after a stop request (the stop is recorded, the goroutine does not return).
// It is both the scheduler's executor and the watchdog's teardown.
type wedgedExecutor struct {
	mu       sync.Mutex
	stops    []string
	cancels  int
	notified []string
	started  chan string
	stopped  chan string
}

func (e *wedgedExecutor) ExecuteWithContext(_ context.Context, taskID string) error {
	e.started <- taskID
	return nil
}
func (e *wedgedExecutor) IsExecuting(string) bool { return true }
func (e *wedgedExecutor) Cancel(string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancels++
	return nil
}
func (e *wedgedExecutor) CancelIfActive(taskID string) (bool, error) {
	e.mu.Lock()
	e.stops = append(e.stops, taskID)
	e.mu.Unlock()
	e.stopped <- taskID
	return true, nil
}
func (e *wedgedExecutor) NotifyChildTerminal(_ context.Context, taskID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notified = append(e.notified, taskID)
}
func (e *wedgedExecutor) CancelChildren(context.Context, string) {}

func (e *wedgedExecutor) snapshot() (stops []string, cancels int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.stops...), e.cancels
}

// stuckExecRepo is the watchdog's execution table: one RUNNING execution for
// the task, last updated an hour ago, until the watchdog records it failed.
type stuckExecRepo struct {
	mu       sync.Mutex
	row      *persistence.Execution
	failures int
	reason   string
}

func (r *stuckExecRepo) List(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures > 0 {
		return nil, nil // no longer RUNNING
	}
	cp := *r.row
	return []*persistence.Execution{&cp}, nil
}
func (r *stuckExecRepo) RecordFailure(_ context.Context, _ string, msg, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures++
	r.reason = msg
	return nil
}
func (r *stuckExecRepo) SupersedeOrphanPausedExecutions(context.Context) (int64, error) {
	return 0, nil
}

// audit 2026-10-12: outcome-quality §S3 #1 (GitHub #142), end to end — "a
// hung execution keeps its scheduler slot and its container after the
// watchdog fails it". A real scheduler over a real SQLite task table, a real
// watchdog, and an executor whose goroutine never returns. 05-scheduler
// §4.12: the watchdog fails the task (stuck: its lease is live) and asks for
// the stop exactly once through the primitive; the scheduler frees the slot
// once the goroutine has outlived its row by the grace; a second scan stops
// nothing. The scheduler's clock and ticks are injected; the watchdog runs one
// scan per Start/Stop.
func TestHungExecution_FailedFreedStoppedOnce(t *testing.T) {
	db := sqlitetest.File(t, "hung142.db")
	tasks := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := tasks.Create(ctx, &persistence.Task{ID: "task_hung", ProjectID: "alpha", Status: persistence.TaskStatusQueued,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	ex := &wedgedExecutor{started: make(chan string, 1), stopped: make(chan string, 4)}
	clock := newFakeClock()
	s := NewWithOptions(tasks, &Config{MaxConcurrency: 1, LeaseDurationSeconds: 60, PollInterval: time.Minute,
		AbandonedExecutionGrace: 10 * time.Minute}, WithExecutor(ex))
	s.now = clock.Now
	ticks := make(chan time.Time)
	s.watchTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	iterated := make(chan struct{}, 1)
	s.watchIterated = func() { iterated <- struct{}{} }
	dispatched := make(chan struct{})
	go func() { s.schedule(); s.dispatchWg.Wait(); close(dispatched) }()
	<-ex.started
	<-iterated

	execs := &stuckExecRepo{row: &persistence.Execution{ID: "exec_hung", TaskID: "task_hung", ProjectID: "alpha",
		Status: persistence.ExecutionStatusRunning, UpdatedAt: now.Add(-time.Hour)}}
	wcfg := watchdog.DefaultConfig()
	wcfg.Interval = time.Hour
	wd := watchdog.New(wcfg, execs, tasks, zerolog.Nop(), nil).WithTaskTeardown(ex)
	if err := wd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ex.stopped:
	case <-time.After(10 * time.Second): // failure guard only
		t.Fatal("the watchdog never asked for the stuck execution to stop")
	}
	_ = wd.Stop()

	got, err := tasks.Get(ctx, "task_hung")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != persistence.TaskStatusFailed || got.LastErrorClass == nil ||
		*got.LastErrorClass != persistence.TaskFailureClassStuckExecution {
		t.Fatalf("the watchdog must fail the task STUCK_EXECUTION, got %s %v", got.Status, got.LastErrorClass)
	}
	if !strings.Contains(execs.reason, "lease is still renewed") {
		t.Fatalf("a task whose lease is live is stuck, not abandoned: %q", execs.reason)
	}

	tick := func(what string) {
		t.Helper()
		ticks <- clock.Now()
		select {
		case <-iterated:
		case <-dispatched:
			t.Fatalf("the dispatch returned (%s)", what)
		}
	}
	clock.Advance(60 * time.Second)
	tick("renewal misses: the row left RUNNING")
	clock.Advance(9 * time.Minute)
	tick("inside the grace")
	clock.Advance(2 * time.Minute)
	ticks <- clock.Now()
	select {
	case <-dispatched:
	case <-iterated:
		t.Fatal("the wedged goroutine kept the slot past the grace")
	}
	s.mu.Lock()
	running := s.runningCount
	s.mu.Unlock()
	if running != 0 {
		t.Fatalf("slot not freed: runningCount=%d", running)
	}

	// A second scan finds nothing to stop.
	if err := wd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = wd.Stop()
	stops, cancels := ex.snapshot()
	if len(stops) != 1 || stops[0] != "task_hung" {
		t.Fatalf("the container must be asked to stop exactly once, stops=%v", stops)
	}
	if cancels != 0 {
		t.Fatalf("the scheduler must not tear down a second time, Cancel called %d times", cancels)
	}
	if got, _ := tasks.Get(ctx, "task_hung"); got.Status != persistence.TaskStatusFailed {
		t.Fatalf("the row's writer owns it; got %s", got.Status)
	}
}

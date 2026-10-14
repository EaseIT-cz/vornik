package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
)

// ctxStrictTaskRepo / ctxStrictExecRepo refuse a write on a finished context,
// as database/sql does. The plain mocks ignore ctx, which is how the defect
// below went unseen.
type ctxStrictTaskRepo struct{ *MockTaskRepo }

func (r ctxStrictTaskRepo) Get(ctx context.Context, id string) (*persistence.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.MockTaskRepo.Get(ctx, id)
}

func (r ctxStrictTaskRepo) UpdateIfStatus(ctx context.Context, t *persistence.Task, allowed []persistence.TaskStatus) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return r.MockTaskRepo.UpdateIfStatus(ctx, t, allowed)
}

func (r ctxStrictTaskRepo) UpdateStatus(ctx context.Context, id string, s persistence.TaskStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.MockTaskRepo.UpdateStatus(ctx, id, s)
}

type ctxStrictExecRepo struct{ *MockExecRepo }

func (r ctxStrictExecRepo) RecordFailure(ctx context.Context, id, msg, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.MockExecRepo.RecordFailure(ctx, id, msg, code)
}

func (r ctxStrictExecRepo) UpdateStatus(ctx context.Context, id string, s persistence.ExecutionStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.MockExecRepo.UpdateStatus(ctx, id, s)
}

// TestHandleFailure_RecordsOnAnExpiredContext — audit 2026-10-12 (GitHub #142,
// outcome-quality LLD §S1 D1): when a workflow's max_wall_clock fired, the
// retry loop called handleFailure with the execution context the cap had just
// expired, the database refused every write, and the errors were discarded.
// The task stayed RUNNING with no last_error: the ceiling fired and the record
// said nothing. The context is expired before the call — deterministic.
func TestHandleFailure_RecordsOnAnExpiredContext(t *testing.T) {
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(NewMockRuntime(), ctxStrictExecRepo{er}, NewMockArtifactRepo(), ctxStrictTaskRepo{tr}, nil)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 1, Attempt: 1, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	// A copy: handleFailure mutates the struct it is handed, and the mock
	// stores pointers, so passing the stored task would fake the write.
	snapshot := *tr.tasks["t1"]
	e.handleFailure(ctx, &snapshot, exec, fmt.Errorf("step s1: %w", context.DeadlineExceeded))

	tr.mu.Lock()
	status := tr.tasks["t1"].Status
	tr.mu.Unlock()
	if status != persistence.TaskStatusFailed {
		t.Errorf("task status = %s, want FAILED: the wall-clock failure was never recorded", status)
	}
	er.mu.Lock()
	got := er.execs["e1"]
	er.mu.Unlock()
	if got.Status != persistence.ExecutionStatusFailed || got.ErrorMessage == nil {
		t.Errorf("execution = %s (error %v), want FAILED with the error recorded", got.Status, got.ErrorMessage)
	}
}

// TestCancelBranch_ReadsAndWritesOnTheCancelledContext — audit 2026-10-12
// (outcome-quality LLD §S1 D1, second half): the cancel branch of the run loop
// asked isTaskCancelled on the context the cancel had just ended, the read
// failed, so it answered false and a genuine operator cancel fell through to
// the failure path; handleCancelled, reached with the same context, could not
// write the CANCELLED status it exists to write.
func TestCancelBranch_ReadsAndWritesOnTheCancelledContext(t *testing.T) {
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(NewMockRuntime(), ctxStrictExecRepo{er}, NewMockArtifactRepo(), ctxStrictTaskRepo{tr}, nil)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusCancelled, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !e.isTaskCancelled(ctx, "t1") {
		t.Fatal("isTaskCancelled answered false for a CANCELLED task because its context had ended")
	}
	snapshot := *tr.tasks["t1"]
	e.handleCancelled(ctx, &snapshot, exec)
	er.mu.Lock()
	got := er.execs["e1"].Status
	er.mu.Unlock()
	if got != persistence.ExecutionStatusCancelled {
		t.Errorf("execution status = %s, want CANCELLED", got)
	}
}

// deadlineRecordingRuntime records whether each container stop carried a
// deadline. A podman stop on context.Background() blocks for as long as podman
// does; nothing bounds it.
type deadlineRecordingRuntime struct {
	*MockRuntime
	unboundedStops int
	onStop         func()
}

func (r *deadlineRecordingRuntime) StopContainer(ctx context.Context, id string, force bool) error {
	if _, ok := ctx.Deadline(); !ok {
		r.unboundedStops++
	}
	if r.onStop != nil {
		r.onStop()
	}
	return r.MockRuntime.StopContainer(ctx, id, force)
}

// TestPause_StopsTheContainerUnderABound — audit 2026-10-12 (outcome-quality
// LLD §S2 C9, review 2c90 finding 19): the pause path stopped the step's
// container on context.Background(), so a podman that does not answer held
// the pause — and a daemon shutdown, which pauses every execution — forever.
func TestPause_StopsTheContainerUnderABound(t *testing.T) {
	rt := &deadlineRecordingRuntime{MockRuntime: NewMockRuntime()}
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})
	_ = er.Create(context.Background(), &persistence.Execution{ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusRunning})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.mu.Lock()
	e.ctx = ctx
	e.cancel = cancel
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1", containerID: "c1", cancel: cancel}
	e.mu.Unlock()
	// The stopped step's goroutine would deregister itself; do it here so the
	// pause's wait for that cleanup returns at once instead of after 30s.
	var once sync.Once
	rt.onStop = func() { once.Do(func() { go e.cleanupExecution("t1") }) }

	if _, err := e.Pause("t1"); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	stops := rt.stopCalls
	rt.mu.Unlock()
	if stops == 0 {
		t.Fatal("pause did not stop the container")
	}
	if rt.unboundedStops != 0 {
		t.Errorf("%d container stop(s) ran with no deadline", rt.unboundedStops)
	}
}

// ctxStrictOutcomeRepo refuses a row on a finished context, as the database does.
type ctxStrictOutcomeRepo struct{ *stubStepOutcomeRepo }

func (r ctxStrictOutcomeRepo) Record(ctx context.Context, o *persistence.ExecutionStepOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.stubStepOutcomeRepo.Record(ctx, o)
}

// TestRecordStepOutcome_WritesAfterTheStepContextEnded — audit 2026-10-12
// (GitHub #142 item 1: "it writes no outcome row, so counters stay blind to
// it"). A step ended by max_wall_clock or an operator cancel records its
// outcome from a defer on the very context that just ended; the row was
// refused and lost, so the timeout and cancelled classes were never counted
// for exactly the steps they exist for.
func TestRecordStepOutcome_WritesAfterTheStepContextEnded(t *testing.T) {
	for name, mk := range map[string]func() (context.Context, context.CancelFunc){
		"cancelled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		},
		"expired": func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Unix(0, 0))
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := mk()
			defer cancel()
			stub := newStubStepOutcomeRepo()
			e := &Executor{outcomeRepo: ctxStrictOutcomeRepo{stub}, logger: zerolog.Nop()}
			e.recordStepOutcome(ctx, &persistence.Task{ID: "t1", ProjectID: "p1"}, &persistence.Execution{ID: "e1"},
				"step_0", "coder", "m", "timeout", "context_timeout", "deadline", nil, nil)
			if len(stub.rows) != 1 {
				t.Fatalf("outcome rows = %d, want 1: the step's ending was never recorded", len(stub.rows))
			}
		})
	}
}

// TestShouldRetry_WarmWaitDeadlineIsNotRetried pins the executor side of C2:
// the warm-pool wait now wraps its context error, and the executor's own
// wrapping (markRetryable) keeps it on the chain, so a warm step that used its
// whole step timeout is declined for a task retry like a cold one.
func TestShouldRetry_WarmWaitDeadlineIsNotRetried(t *testing.T) {
	e, _, _, _, _ := setup()
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	_, werr := (&runtime.WarmPool{}).WaitForTaskDone(ctx, &runtime.PoolEntry{OutputDir: t.TempDir()}, time.Minute)
	err := markRetryable(fmt.Errorf("warm container task failed: %w", werr))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("warm wait error lost its cause: %v", err)
	}
	if e.shouldRetry(err) {
		t.Errorf("a warm step that ran out its step timeout was marked for a task retry: %v", err)
	}
}

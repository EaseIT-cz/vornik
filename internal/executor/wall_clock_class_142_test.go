package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// expiredWallClock returns an execution context its wall-clock cap has
// already ended — deterministic: the deadline is in the past.
func expiredWallClock(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := wallClockContext(context.Background(), -time.Second, wallClockSourceWorkflow)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

// TestWallClockContext_RecordsItsCause — audit 2026-10-12, GitHub #142 §S7
// #2: the cap was a plain context.WithTimeout, so nothing downstream could
// tell it from a step's own timeout.
func TestWallClockContext_RecordsItsCause(t *testing.T) {
	ctx := expiredWallClock(t)
	c := wallClockCauseOf(ctx)
	if c == nil {
		t.Fatal("the wall-clock cap left no cause on the execution context")
	}
	if c.source != wallClockSourceWorkflow {
		t.Errorf("source = %q", c.source)
	}
	// A descendant (the step's context) sees the same cause.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if wallClockCauseOf(child) == nil {
		t.Error("a context derived from the execution context lost the cause")
	}
	// A context that ended for another reason has none.
	other, cancelOther := context.WithTimeout(context.Background(), -time.Second)
	defer cancelOther()
	if wallClockCauseOf(other) != nil {
		t.Error("a plain deadline was read as the wall-clock cap")
	}
}

// TestFailedStepOutcome_WallClockIsNotTheStepTimeout — audit 2026-10-12,
// GitHub #142 §S7 #2: a step the execution's wall clock ended was recorded
// timeout/context_timeout, the step-timeout pair, which drives an inert
// step-timeout raise and an infra-ladder re-run on a dead context.
func TestFailedStepOutcome_WallClockIsNotTheStepTimeout(t *testing.T) {
	deadline := fmt.Errorf("podman wait timed out after 30m0s: %w", context.DeadlineExceeded)

	outcome, class := failedStepOutcome(expiredWallClock(t), deadline, agentStops{})
	if outcome != string(stepoutcome.Timeout) || class != stepoutcome.ClassWallClockExceeded {
		t.Errorf("wall clock: (%s, %s), want (timeout, wall_clock_exceeded)", outcome, class)
	}

	// The step's own timeout, on a live execution context, keeps its pair.
	outcome, class = failedStepOutcome(context.Background(), deadline, agentStops{})
	if outcome != string(stepoutcome.Timeout) || class != stepoutcome.ClassContextTimeout {
		t.Errorf("step timeout: (%s, %s), want (timeout, context_timeout)", outcome, class)
	}

	// A cause the agent named still outranks the clock.
	loop := fmt.Errorf("degenerate loop detected; then: %w", context.DeadlineExceeded)
	if _, class = failedStepOutcome(expiredWallClock(t), loop, agentStops{}); class != stepoutcome.ClassDegenerateLoop {
		t.Errorf("named cause under the wall clock: class %s, want degenerate_loop", class)
	}

	if stepoutcome.IsInfraRetryableClass(stepoutcome.ClassWallClockExceeded) {
		t.Error("wall_clock_exceeded is infra-retryable: the ladder would re-run on a dead context")
	}
	if persistence.StepClassWallClockExceeded != stepoutcome.ClassWallClockExceeded {
		t.Error("persistence and stepoutcome spell the wall-clock class differently")
	}
	if persistence.CountsAsStepTruncation("timeout", stepoutcome.ClassWallClockExceeded) {
		t.Error("a wall-clock stop counts as a truncation: it would drive an inert step-timeout raise")
	}
	if !persistence.CountsAsStepTruncation("timeout", stepoutcome.ClassContextTimeout) {
		t.Error("a step timeout no longer counts as a truncation")
	}
}

// TestSystemStep_WallClockIsItsOwnClass — the system arm of §S7 #2.
func TestSystemStep_WallClockIsItsOwnClass(t *testing.T) {
	stepCtx, cancel := systemStepContext(expiredWallClock(t), time.Hour)
	defer cancel()
	outcome, class, err := systemStepFailure(stepCtx, context.DeadlineExceeded, time.Hour)
	if outcome != string(stepoutcome.Timeout) || class != stepoutcome.ClassWallClockExceeded {
		t.Errorf("(%s, %s), want (timeout, wall_clock_exceeded)", outcome, class)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v lost DeadlineExceeded", err)
	}
}

// TestHandleFailure_NamesTheWallClockCap — §S7 #2: the task's last_error says
// the execution's cap ran out, with its value and where it came from, so the
// operator raises the cap and not a step timeout.
func TestHandleFailure_NamesTheWallClockCap(t *testing.T) {
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(NewMockRuntime(), ctxStrictExecRepo{er}, NewMockArtifactRepo(), ctxStrictTaskRepo{tr}, nil)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 1, Attempt: 1, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	ctx, cancel := wallClockContext(context.Background(), -time.Second, wallClockSourceDefault)
	defer cancel()
	<-ctx.Done()
	snapshot := *tr.tasks["t1"]
	e.handleFailure(ctx, &snapshot, exec, fmt.Errorf("step s1: %w", context.DeadlineExceeded))

	tr.mu.Lock()
	got := tr.tasks["t1"]
	tr.mu.Unlock()
	if got.LastError == nil || !strings.Contains(*got.LastError, "wall-clock cap") ||
		!strings.Contains(*got.LastError, "scheduler.execution_timeout") {
		t.Errorf("last_error = %v, want it to name the wall-clock cap and its source", got.LastError)
	}
	if got.LastErrorClass == nil || *got.LastErrorClass != persistence.TaskFailureClassTimeout {
		t.Errorf("last_error_class = %v, want TIMEOUT", got.LastErrorClass)
	}
}

package executor

// GitHub #837 (outcome-quality design, amendment 2026-11-20): an agent step
// whose container wait ends because the step's own budget elapsed says the
// step ran out of time, not that waiting for the container failed. The
// classes do not move: the chain still carries context.DeadlineExceeded.
//
// Found at: e2e-lane (24 of 196 calibration runs read "failed waiting for
// container: podman wait timed out", taken for a host fault in the #809
// summary). Regression test at: unit. Lower because: the step's wait and its
// error text are decided here; the e2e-lane only quotes them.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// blockingWait answers WaitForExit as runtime.Manager does when its context
// ends: "podman wait timed out after <timeout>: <ctx.Err()>".
type blockingWait struct{ *MockRuntime }

func (blockingWait) WaitForExit(ctx context.Context, _ string, timeout time.Duration) (int, error) {
	<-ctx.Done()
	return -1, fmt.Errorf("podman wait timed out after %s: %w", timeout, ctx.Err())
}

type failingWait struct{ *MockRuntime }

func (failingWait) WaitForExit(context.Context, string, time.Duration) (int, error) {
	return -1, errors.New("podman wait failed: exit status 1")
}

func TestWaitError837_TheStepsOwnBudgetSaysSo(t *testing.T) {
	e := &Executor{runtime: blockingWait{&MockRuntime{}}}
	ctx := context.Background()
	_, err := e.waitForCompletion(ctx, "c1", 20*time.Millisecond)
	got := agentWaitError(ctx, "implement", err)
	if !strings.HasPrefix(got.Error(), "step implement ran out of its 20ms time budget (the container was stopped): ") {
		t.Fatalf("text = %q", got)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatal("the chain lost DeadlineExceeded, so the class would move")
	}
}

func TestWaitError837_TheWallClockSaysSo(t *testing.T) {
	e := &Executor{runtime: blockingWait{&MockRuntime{}}}
	ctx, cancel := wallClockContext(context.Background(), 10*time.Millisecond, "workflow max_wall_clock")
	defer cancel()
	_, err := e.waitForCompletion(ctx, "c1", time.Minute)
	got := agentWaitError(ctx, "implement", err)
	if !strings.HasPrefix(got.Error(), "step implement was stopped because the execution's wall-clock cap (10ms, from workflow max_wall_clock) ran out (the container was stopped): ") {
		t.Fatalf("text = %q", got)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatal("the chain lost DeadlineExceeded")
	}
}

func TestWaitError837_AFailedWaitKeepsItsText(t *testing.T) {
	e := &Executor{runtime: failingWait{&MockRuntime{}}}
	ctx := context.Background()
	_, err := e.waitForCompletion(ctx, "c1", time.Minute)
	got := agentWaitError(ctx, "implement", err)
	if got.Error() != "failed waiting for container: podman wait failed: exit status 1" {
		t.Fatalf("text = %q", got)
	}
}

// innerTimerWait models WaitForExit's own timer of the same length firing
// before the caller's: the caller's context is still live, and the error is a
// deadline (review 47eb F1).
type innerTimerWait struct{ *MockRuntime }

func (innerTimerWait) WaitForExit(_ context.Context, _ string, timeout time.Duration) (int, error) {
	return -1, fmt.Errorf("podman wait timed out after %s: %w", timeout, context.DeadlineExceeded)
}

func TestWaitError837_TheInnerTimerFiringFirstIsStillTheBudget(t *testing.T) {
	e := &Executor{runtime: innerTimerWait{&MockRuntime{}}}
	ctx := context.Background()
	_, err := e.waitForCompletion(ctx, "c1", 5*time.Minute)
	if got := agentWaitError(ctx, "implement", err); !strings.HasPrefix(got.Error(), "step implement ran out of its 5m0s time budget") {
		t.Fatalf("text = %q", got)
	}
}

// A cancelled caller is neither deadline: the plain text, and the chain says
// Canceled (the 2026-10-12 regression: a cancel read as a timeout).
func TestWaitError837_ACancelIsNeitherDeadline(t *testing.T) {
	e := &Executor{runtime: blockingWait{&MockRuntime{}}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err := e.waitForCompletion(ctx, "c1", time.Minute)
	got := agentWaitError(ctx, "implement", err)
	if !strings.HasPrefix(got.Error(), "failed waiting for container: ") || !errors.Is(got, context.Canceled) {
		t.Fatalf("text = %q", got)
	}
}

// The step's budget firing inside a live wall clock is the budget.
func TestWaitError837_TheBudgetWinsInsideALiveWallClock(t *testing.T) {
	e := &Executor{runtime: blockingWait{&MockRuntime{}}}
	ctx, cancel := wallClockContext(context.Background(), time.Minute, "workflow max_wall_clock")
	defer cancel()
	_, err := e.waitForCompletion(ctx, "c1", 10*time.Millisecond)
	if got := agentWaitError(ctx, "implement", err); !strings.HasPrefix(got.Error(), "step implement ran out of its 10ms time budget") {
		t.Fatalf("text = %q", got)
	}
}

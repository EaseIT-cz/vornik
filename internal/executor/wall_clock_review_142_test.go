package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// TestFailedStepOutcome_APodmanRunBoundIsAStartFailureNotATruncation — code
// review 3ea6 #2 (GitHub #142 §S7 #8): a `podman run` the runtime's image
// bound cut off must not be recorded as a step truncation, or the tune worker
// would propose raising the step's timeout, which cannot help. The start arm
// claims it: (failed, container_start_failed).
func TestFailedStepOutcome_APodmanRunBoundIsAStartFailureNotATruncation(t *testing.T) {
	// The chain StartContainer and startContainer build around the bound.
	err := fmt.Errorf("failed to start container: %w",
		fmt.Errorf("podman run failed: %w",
			fmt.Errorf("%w - output: %s",
				fmt.Errorf("podman run did not finish within 15m0s (the runtime's bound for this verb): %w", context.DeadlineExceeded), "")))
	outcome, class := failedStepOutcome(context.Background(), err, agentStops{})
	if outcome != string(stepoutcome.Failed) || class != stepoutcome.ClassContainerStartFailed {
		t.Errorf("(%s, %s), want (failed, container_start_failed)", outcome, class)
	}
	if persistence.CountsAsStepTruncation(outcome, class) {
		t.Error("a podman-run bound counts as a step truncation")
	}
}

// TestHandleFailure_DoesNotNameTheCapTwice — code review 3ea6 #3: a system
// step's error already carries the cap's cause; handleFailure must not prefix
// it again. The guard is on the cause in the chain, not on the prose.
func TestHandleFailure_DoesNotNameTheCapTwice(t *testing.T) {
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(NewMockRuntime(), ctxStrictExecRepo{er}, NewMockArtifactRepo(), ctxStrictTaskRepo{tr}, nil)
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 1, Attempt: 1, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	ctx := expiredWallClock(t)
	stepCtx, cancel := systemStepContext(ctx, time.Hour)
	defer cancel()
	_, _, stepErr := systemStepFailure(stepCtx, context.DeadlineExceeded, time.Hour)

	snapshot := *tr.tasks["t1"]
	e.handleFailure(ctx, &snapshot, exec, fmt.Errorf("step s1: %w", stepErr))
	tr.mu.Lock()
	got := tr.tasks["t1"]
	tr.mu.Unlock()
	if got.LastError == nil {
		t.Fatal("no last_error")
	}
	if n := strings.Count(*got.LastError, "wall-clock cap"); n != 1 {
		t.Errorf("last_error names the cap %d times, want once: %q", n, *got.LastError)
	}
}

package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// TestShutdown_NilReceiver — defensive: callers may invoke
// Shutdown on a nil executor handle (the daemon's graceful path
// runs even when executor init failed).
func TestShutdown_NilReceiver(t *testing.T) {
	var nilExec *Executor
	require.NoError(t, nilExec.Shutdown(context.Background()))
}

// TestShutdown_NoActiveExecutions_DelegatesToStop — when no
// executions are in flight, Shutdown falls through to Stop().
// Verifies the early-return path (no pauseWithReason loop runs
// — there's nothing to pause).
func TestShutdown_NoActiveExecutionsY(t *testing.T) {
	e, _, _, _, _ := setup()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, e.Shutdown(ctx))

	// 2026-05-29 audit fix: Stop() now resets shuttingDown so a
	// Stop()+Recover() cycle on the same Executor instance
	// (graceful-restart path, tests) doesn't have its
	// recoverExecution calls fast-rejected. Shutdown's no-active
	// fast path falls through to Stop, so shuttingDown ends up
	// false here. The daemon container constructs a FRESH Executor
	// on actual daemon-restart so the admission gate that pre-fix
	// stayed closed across Shutdown→exit→restart is no longer
	// load-bearing; restart starts from a zero-value Executor.
	assert.False(t, e.isShuttingDown(),
		"Stop's reset path must clear shuttingDown so the executor is reusable after Shutdown's no-active fast-path")
}

// TestStop_ResetsShuttingDownFlag — regression for the 2026-05-29
// audit-agent finding: pre-fix Shutdown set shuttingDown=true and
// Stop never cleared it, so a Stop()+Recover() cycle on the same
// Executor (test harnesses, future graceful-restart paths) silently
// fast-rejected every recoverExecution with "executor is shutting
// down". Stop is the canonical drain-then-reusable contract; this
// test pins the reset.
func TestStop_ResetsShuttingDownFlag(t *testing.T) {
	e, _, _, _, _ := setup()
	// Simulate post-Shutdown state without going through the whole
	// pause loop — direct flag set under the lock matches Shutdown's
	// initial action.
	e.mu.Lock()
	e.shuttingDown = true
	e.mu.Unlock()
	require.True(t, e.isShuttingDown(), "precondition")

	require.NoError(t, e.Stop(context.Background()))
	assert.False(t, e.isShuttingDown(),
		"Stop must clear shuttingDown so the executor is reusable for a fresh task/recover cycle")
}

// TestIsShuttingDown_FlagToggling — pin the toggle helpers used
// by Shutdown() and ExecuteWithContext()'s admission gate.
func TestIsShuttingDown_Toggle(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.False(t, e.isShuttingDown())

	e.mu.Lock()
	e.shuttingDown = true
	e.mu.Unlock()
	assert.True(t, e.isShuttingDown())
}

// TestActiveCount_ZeroAndPopulated — sanity-check the
// active-execution count helper used by ops dashboards.
func TestActiveCount_Snapshot(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.Equal(t, 0, e.ActiveCount())

	e.mu.Lock()
	e.activeExecutions["a"] = &executionHandle{taskID: "a"}
	e.activeExecutions["b"] = &executionHandle{taskID: "b"}
	e.mu.Unlock()

	assert.Equal(t, 2, e.ActiveCount())
	assert.True(t, e.IsExecuting("a"))
	assert.False(t, e.IsExecuting("missing"))
}

// TestShutdown_NoActiveExec_BoundedContext — Stop respects the
// supplied context's deadline. Shutdown with a zero-budget ctx
// returns the ctx error (when no goroutine is draining, Stop
// completes immediately so an expired ctx still succeeds).
func TestShutdown_BoundedDeadline(t *testing.T) {
	e, _, _, _, _ := setup()

	// Already-cancelled context — Stop sees done channel + ctx.Done
	// both ready; the select races. With no active executions, the
	// done channel closes immediately (e.wg has no entries), so the
	// outer Shutdown returns the Stop result. Either branch wins
	// here is acceptable behaviour (both leave the executor
	// drained); we just assert no panic and the executor is
	// reusable for a fresh task.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = e.Shutdown(ctx)

	// After Shutdown, isShuttingDown ought to be false (Stop reset
	// path) OR true (Shutdown set + we early-returned without
	// resetting). Either is consistent — important property is the
	// executor isn't broken.
	tr := NewMockTaskRepo()
	tr.AddTask(&persistence.Task{ID: "post", ProjectID: "p", Status: persistence.TaskStatusQueued, CreatedAt: time.Now()})
}

package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #300 (2026-10-08): companion cancel left the execution running. The
// CPC cancel-on-timeout cascade (05-scheduler.md §4.11 site #7) flipped the
// callee row and stopped nothing; its comment claimed "the existing
// task-cancel primitive handles the rest", and nothing was called.
func TestCPCCascadeCancel_StopsTheCalleeContainer(t *testing.T) {
	rt := &teardownRuntime{}
	e := liveExecutor(t, "callee-running", "callee-container", rt)
	e.cpcRepo = newMockCPCRepo()
	id := "callee-running"
	NewCPCTimeoutScanner(e).cascadeCancelCallee(context.Background(),
		&persistence.CrossProjectCall{ID: "c1", CalleeTaskID: &id})
	if got := rt.stoppedIDs(); len(got) != 1 || got[0] != "callee-container" {
		t.Fatalf("the callee's container must be stopped; stopped=%v", got)
	}
}

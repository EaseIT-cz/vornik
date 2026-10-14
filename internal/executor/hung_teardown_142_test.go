package executor

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: outcome-quality §S3 #1 (GitHub #142). The watchdog now
// tears a stuck execution down through the cancel primitive AFTER it wrote the
// task and the execution FAILED (STUCK_EXECUTION). Cancel — behind
// CancelIfActive — wrote both rows CANCELLED with plain UpdateStatus, so that
// teardown would have rewritten the watchdog's record. 05-scheduler §4.12 C4:
// Cancel's writes are conditional on a non-terminal status; it still stops the
// container, once.
func TestCancel_DoesNotOverwriteATerminalRow(t *testing.T) {
	rt := &teardownRuntime{}
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	tr.AddTask(&persistence.Task{ID: "task-1", ProjectID: "p1", Status: persistence.TaskStatusFailed})
	if err := er.Create(context.Background(), &persistence.Execution{ID: "exec-1", TaskID: "task-1", ProjectID: "p1",
		Status: persistence.ExecutionStatusFailed}); err != nil {
		t.Fatal(err)
	}
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	e.mu.Lock()
	e.activeExecutions["task-1"] = &executionHandle{taskID: "task-1", projectID: "p1", containerID: "c-1", cancel: func() {}}
	e.mu.Unlock()

	had, err := e.CancelIfActive("task-1")
	if !had || err != nil {
		t.Fatalf("a live execution must be stopped: had=%v err=%v", had, err)
	}
	if got := rt.stoppedIDs(); len(got) != 1 || got[0] != "c-1" {
		t.Fatalf("the container must be stopped exactly once, stopped=%v", got)
	}
	task, _ := tr.Get(context.Background(), "task-1")
	if task.Status != persistence.TaskStatusFailed {
		t.Errorf("the teardown rewrote the task's terminal record: %s", task.Status)
	}
	if st := er.snapshotStatus("exec-1"); st != persistence.ExecutionStatusFailed {
		t.Errorf("the teardown rewrote the execution's terminal record: %s", st)
	}
}

// The other half of the same rule: when the unwound goroutine finalises and
// defers to a task another writer failed, it must not rewrite that writer's
// FAILED execution row as CANCELLED either (05-scheduler §4.12 C4).
func TestDeferToForeignTerminal_KeepsAFailedExecution(t *testing.T) {
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	tr.AddTask(&persistence.Task{ID: "task-1", ProjectID: "p1", Status: persistence.TaskStatusFailed})
	exec := &persistence.Execution{ID: "exec-1", TaskID: "task-1", ProjectID: "p1", Status: persistence.ExecutionStatusFailed}
	if err := er.Create(context.Background(), exec); err != nil {
		t.Fatal(err)
	}
	e := NewWithOptions(&teardownRuntime{}, er, NewMockArtifactRepo(), tr, nil)

	if !e.deferToForeignTerminal(context.Background(), &persistence.Task{ID: "task-1"}, exec, "test") {
		t.Fatal("a FAILED task written by another writer must be deferred to")
	}
	if st := er.snapshotStatus("exec-1"); st != persistence.ExecutionStatusFailed {
		t.Errorf("the finaliser rewrote another writer's FAILED execution as %s", st)
	}
}

// audit 2026-10-12: outcome-quality §S3 #1 (GitHub #142) — the scheduler's
// ExecutionTimeout was configured and never read, and a workflow with no
// maxWallClock had no ceiling at all. 05-scheduler §4.12 C1: the daemon value
// is the executor's default wall-clock cap; a workflow's own maxWallClock
// always wins, larger or smaller; with neither there is no cap.
func TestWallClockCap_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		workflow string
		def      time.Duration
		want     time.Duration
		source   string
	}{
		{"workflow wins when smaller", "15m", 2 * time.Hour, 15 * time.Minute, wallClockSourceWorkflow},
		{"workflow wins when larger", "72h", 2 * time.Hour, 72 * time.Hour, wallClockSourceWorkflow},
		{"default fills an unset workflow", "", 2 * time.Hour, 2 * time.Hour, wallClockSourceDefault},
		{"default fills an unparseable workflow", "soon", 2 * time.Hour, 2 * time.Hour, wallClockSourceDefault},
		{"neither: no cap", "", 0, 0, ""},
		{"workflow alone", "30m", 0, 30 * time.Minute, wallClockSourceWorkflow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, source, _ := effectiveWallClock(c.workflow, c.def)
			if got != c.want || source != c.source {
				t.Fatalf("effectiveWallClock(%q, %s) = %s, %q; want %s, %q", c.workflow, c.def, got, source, c.want, c.source)
			}
		})
	}
}

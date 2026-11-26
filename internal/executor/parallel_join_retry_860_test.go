package executor

// GitHub #860 (parallel fan-out design, "Amendment 2026-11-25"): the
// parallel_join row is keyed on (task_id, step_id), v0.6 rule 3, not per
// attempt. This pins what a retried task records: a later attempt whose join
// also proceeds writes no second row, and the first attempt's counts stand.
// The store itself stays append-only; this guard is the only idempotent write.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func TestParallelJoin860_ARetriedTaskKeepsTheFirstAttemptsRow(t *testing.T) {
	e, _, _, _, tr := setup()
	stub := newStubStepOutcomeRepo()
	e.outcomeRepo = stub
	task := &persistence.Task{ID: "par", ProjectID: "p1", Attempt: 1}
	tr.AddTask(task)
	addParallelChildren(tr, []persistence.TaskStatus{persistence.TaskStatusCompleted, persistence.TaskStatusCompleted})
	step := registry.WorkflowStep{Type: "parallel", Join: "j", JoinPolicy: "all"}

	e.emitParallelJoinOutcome(context.Background(), task, &persistence.Execution{ID: "ex1", TaskID: "par", ProjectID: "p1"}, "fanout", step)

	// A later step failed, the task retried: attempt 2 fans out three new legs
	// and its join proceeds too.
	task.Attempt = 2
	pid, mode := "par", persistence.DelegationModeParallel
	for i := 0; i < 3; i++ {
		payload, _ := json.Marshal(map[string]any{"parallel_step_id": "fanout", "parent_attempt": 2})
		tr.AddTask(&persistence.Task{ID: "par-a2c" + string(rune('0'+i)), ParentTaskID: &pid, DelegationMode: &mode, Status: persistence.TaskStatusCompleted, Payload: payload})
	}
	e.emitParallelJoinOutcome(context.Background(), task, &persistence.Execution{ID: "ex2", TaskID: "par", ProjectID: "p1"}, "fanout", step)

	rows := parallelJoinRows(stub)
	if len(rows) != 1 {
		t.Fatalf("parallel_join rows = %d, want 1 per task (v0.6 rule 3)", len(rows))
	}
	if rows[0].ExecutionID != "ex1" || !strings.Contains(rows[0].ErrorDetail, `"total":2`) {
		t.Errorf("row = %s %s, want attempt 1's row (ex1, total 2)", rows[0].ExecutionID, rows[0].ErrorDetail)
	}
}

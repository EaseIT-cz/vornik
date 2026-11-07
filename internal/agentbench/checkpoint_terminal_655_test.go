package agentbench

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// A checkpoint is not a finish (benchmark LLD §12.29, GitHub #655).
//
// Incident, found at production (operator observation of the 2026-11-05/06
// recalibration, compared here as the e2e-lane bench run): 26 of 132 task runs
// journaled succeeded had ended at dev-pipeline's `checkpoint` terminal, a
// COMPLETED terminal declared recovery: true, with review.all_done=false. One
// of them (lh-06 r2) never wrote its target at all. The harness read task
// status alone, so a feature left half done read as a pass.
//
// Lower because: the defect is the harness's success rule, which these
// runner tests drive through the same Runner.Run the bench calls; the SQL read
// it depends on is tested against a real SQLite ledger below, and the
// end-to-end run is the smoke re-run that follows the fix.

func TestRunner_ACheckpointTerminalIsNotASuccess_655(t *testing.T) {
	traces := &fakeTraces{terminals: map[string]ExecutionTerminal{
		"t1-e1": {StepID: "checkpoint", Recovery: true},
	}}
	r := &Runner{Tasks: &fakeTasks{}, Traces: traces}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1", Prompt: "p"}}
	cfg.Repeats = 1

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	run := j.TaskRuns[0]
	if run.Succeeded {
		t.Fatalf("a run that ended at a recovery terminal must not be journaled as succeeded: %+v", run)
	}
	if run.Terminal != "checkpoint" {
		t.Fatalf("the run must carry the terminal it reached: %+v", run)
	}
	if !strings.Contains(run.ErrorText, `recovery terminal "checkpoint"`) {
		t.Fatalf("the error must say why: %q", run.ErrorText)
	}
	if got := ClassifyFailure(run.Succeeded, run.ErrorText); got != FailureTask {
		t.Fatalf("an unfinished feature is the agent's failure, got %s", got)
	}
	for _, rec := range j.Records {
		if rec.Succeeded {
			t.Fatalf("every execution record must agree with the task run: %+v", rec)
		}
	}
}

func TestRunner_ANonRecoveryTerminalStaysASuccess_655(t *testing.T) {
	traces := &fakeTraces{terminals: map[string]ExecutionTerminal{
		"t1-e1": {StepID: "complete"},
	}}
	r := &Runner{Tasks: &fakeTasks{}, Traces: traces}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1", Prompt: "p"}}
	cfg.Repeats = 1

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if run := j.TaskRuns[0]; !run.Succeeded || run.Terminal != "complete" {
		t.Fatalf("a run that finished at a non-recovery terminal is a success: %+v", run)
	}
}

// The newest execution decides: executions are ledger-ordered oldest to
// newest, and the task's status is the newest one's.
func TestRunner_TheNewestExecutionDecidesTheTerminal_655(t *testing.T) {
	traces := &fakeTraces{
		execs: []string{"old", "new"},
		terminals: map[string]ExecutionTerminal{
			"old": {StepID: "checkpoint", Recovery: true},
			"new": {StepID: "complete"},
		},
	}
	r := &Runner{Tasks: &fakeTasks{outcomes: map[string]TaskOutcome{
		"t1": {TaskID: "t1", Succeeded: true},
	}}, Traces: traces}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1", Prompt: "p"}}
	cfg.Repeats = 1

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if run := j.TaskRuns[0]; !run.Succeeded || run.Terminal != "complete" {
		t.Fatalf("the newest execution's terminal decides: %+v", run)
	}
}

// An unreadable terminal fails closed, on the harness: the harness cannot say
// the work finished, and it must not charge the model either.
func TestRunner_AnUnreadableTerminalIsAHarnessFailure_655(t *testing.T) {
	traces := &fakeTraces{terminalErr: errors.New("no workflow snapshot")}
	r := &Runner{Tasks: &fakeTasks{}, Traces: traces}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1", Prompt: "p"}}
	cfg.Repeats = 1

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	run := j.TaskRuns[0]
	if run.Succeeded {
		t.Fatalf("an unreadable terminal must not read as success: %+v", run)
	}
	if got := ClassifyFailure(run.Succeeded, run.ErrorText); got != FailureHarness {
		t.Fatalf("an unreadable terminal is the harness's failure, got %s (%q)", got, run.ErrorText)
	}
}

// A task the daemon reported failed is not re-read: its terminal cannot turn
// it into a success, and a failed read must not replace its error text.
func TestRunner_AFailedTaskKeepsItsOwnError_655(t *testing.T) {
	traces := &fakeTraces{terminalErr: errors.New("must not be asked")}
	r := &Runner{Tasks: &fakeTasks{outcomes: map[string]TaskOutcome{
		"t1": {TaskID: "t1", Succeeded: false, ErrorText: "Pipeline failed (last step: implement)"},
	}}, Traces: traces}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1", Prompt: "p"}}
	cfg.Repeats = 1

	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if run := j.TaskRuns[0]; run.Succeeded || run.ErrorText != "Pipeline failed (last step: implement)" {
		t.Fatalf("a failed task keeps the daemon's error: %+v", run)
	}
}

func TestSQLTraceStore_ReadsTheTerminalAndItsRecoveryFlag_655(t *testing.T) {
	db := newLedgerDB(t)
	if _, err := db.Exec(`CREATE TABLE executions (id TEXT, current_step_id TEXT, workflow_snapshot BLOB)`); err != nil {
		t.Fatalf("create executions: %v", err)
	}
	snapshot := `{"ID":"dev-pipeline","Terminals":{"checkpoint":{"Status":"COMPLETED","Recovery":true},"complete":{"Status":"COMPLETED","Recovery":false}}}`
	for _, row := range [][3]any{
		{"e-ckpt", "checkpoint", snapshot},
		{"e-done", "complete", snapshot},
		{"e-undeclared", "somewhere-else", snapshot},
		{"e-nosnap", "complete", nil},
		{"e-nostep", nil, snapshot},
	} {
		if _, err := db.Exec(`INSERT INTO executions VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	store := &SQLTraceStore{DB: db, Dialect: SQLite}
	ctx := context.Background()

	got, err := store.Terminal(ctx, "e-ckpt")
	if err != nil || got != (ExecutionTerminal{StepID: "checkpoint", Recovery: true}) {
		t.Fatalf("checkpoint: %+v, %v", got, err)
	}
	got, err = store.Terminal(ctx, "e-done")
	if err != nil || got != (ExecutionTerminal{StepID: "complete"}) {
		t.Fatalf("complete: %+v, %v", got, err)
	}
	for _, id := range []string{"e-undeclared", "e-nosnap", "e-nostep", "e-missing"} {
		if _, err := store.Terminal(ctx, id); err == nil {
			t.Errorf("%s: a terminal the harness cannot verify must be an error", id)
		}
	}
}

// Both §12.29 rules change what a journal's succeeded means and what a task
// starts from, so journals either side must not compare.
func TestHarnessVersionMovesForTheCheckpointRule_655(t *testing.T) {
	if HarnessVersion != "10" {
		t.Fatalf("HarnessVersion = %q, want 10 (benchmark LLD §12.29)", HarnessVersion)
	}
	if WorkspaceResetTargetsAndState == WorkspaceResetTargets {
		t.Fatal("the reset that also clears workflow state needs its own provenance value")
	}
}

// The terminal check decodes the snapshot into its own two-field shape; the
// daemon writes it from registry.Workflow. This pins the two together.
func TestPinnedTerminals_MatchesTheRegistryShape_655(t *testing.T) {
	raw, err := json.Marshal(registry.Workflow{ID: "w", Terminals: map[string]registry.WorkflowTerminal{
		"checkpoint": {Status: "COMPLETED", Recovery: true},
		"complete":   {Status: "COMPLETED"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var got pinnedTerminals
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	ckpt, ok := got.Terminals["checkpoint"]
	if !ok || !ckpt.Recovery {
		t.Fatalf("registry.Workflow's JSON no longer carries Terminals[...].Recovery as the harness reads it: %s", raw)
	}
	if done, ok := got.Terminals["complete"]; !ok || done.Recovery {
		t.Fatalf("complete must decode as a non-recovery terminal: %s", raw)
	}
}

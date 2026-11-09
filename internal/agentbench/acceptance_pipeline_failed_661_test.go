package agentbench

import (
	"context"
	"strings"
	"testing"
)

// GitHub #661 (benchmark LLD §12.32): on 2026-11-06 sw-02-offbyone r1 stopped
// at review in a degenerate loop and never merged, yet the grader ran against
// HEAD and recorded does_not_compile, the label for code that does not build.

func pipelineRun661(t *testing.T, succeeded, produced bool) (TaskRun, *fakeGrader) {
	t.Helper()
	g := &fakeGrader{}
	tasks := &fakeTasks{outcomes: map[string]TaskOutcome{
		"t": {TaskID: "t", Succeeded: succeeded, ErrorText: "agent entered a degenerate loop", Executions: []string{"t-e1"}},
	}}
	ws := &fakeWorkspace{produced: map[string]bool{"scratch/trunc": produced}}
	r := &Runner{Tasks: tasks, Traces: &fakeTraces{}, Workspace: ws, Grader: g}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t", Prompt: "scratch/trunc", Targets: []string{"scratch/trunc"}, Acceptance: "testdata/acceptance/t"}}
	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(j.TaskRuns) != 1 || j.TaskRuns[0].Acceptance == nil {
		t.Fatalf("want one graded task run: %+v", j.TaskRuns)
	}
	return j.TaskRuns[0], g
}

func TestRunner_FailedRepeatWithNoPackageIsPipelineFailed_661(t *testing.T) {
	run, g := pipelineRun661(t, false, false)
	if run.Acceptance.Outcome != AcceptancePipelineFailed {
		t.Fatalf("a failed repeat whose package never reached HEAD is pipeline_failed, not %q", run.Acceptance.Outcome)
	}
	if !strings.Contains(run.Acceptance.Output, "degenerate loop") {
		t.Fatalf("the outcome carries why the pipeline stopped: %q", run.Acceptance.Output)
	}
	if len(g.graded) != 0 {
		t.Fatalf("nothing reached HEAD to grade, so the grader must not run: %v", g.graded)
	}
}

func TestRunner_FailedRepeatWithItsPackageIsGraded_661(t *testing.T) {
	run, g := pipelineRun661(t, false, true)
	if len(g.graded) != 1 || run.Acceptance.Outcome != AcceptanceFailed {
		t.Fatalf("code that reached HEAD is graded whatever the pipeline said: graded %v, outcome %q", g.graded, run.Acceptance.Outcome)
	}
}

func TestRunner_SucceededRepeatWithNoPackageIsGraded_661(t *testing.T) {
	_, g := pipelineRun661(t, true, false)
	if len(g.graded) != 1 {
		t.Fatalf("a pipeline that reported done is graded, so a missing package stays the grader's verdict: %v", g.graded)
	}
}

package agentadmin

import (
	"reflect"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Design §18.10 (2026-10-03): the engineering--gaps team's thinker wrote
// artifacts/work/cases.md and the critic never saw it (task
// task_20261003001729_083c470d9167367c): only artifacts/out/ carries between
// steps, and no step was told so. Control: the step template, by position.
const (
	handoffSentence = "Only files there reach later steps"
	earlierSentence = "Earlier steps' files are in `artifacts/out/`."
	answerSentence  = "Write `artifacts/out/result.json`"
	reservedName    = "Do not write `artifacts/out/result.json`"
	closingSentence = "Write no other file, and do not change earlier steps' files."
)

func renderSteps(t *testing.T, names ...string) *registry.Workflow {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.project("finance")
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{
		{Name: "reader", Instructions: "Read.", Tools: []string{"file_write"}}}}))
	steps := make([]StepInput, 0, len(names))
	for _, n := range names {
		steps = append(steps, StepInput{Name: n, Role: "reader", Instructions: "Do " + n + "."})
	}
	c := tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "finance", Slug: "spend", Purpose: "spending",
		Steps: steps, Egress: egress1()})
	path := workflowPath(WorkflowID("hermes", "finance", "spend"))
	for _, op := range c.Ops {
		if op.Path == path {
			wf, err := registry.ParseWorkflowMarkdown([]byte(op.Content), path)
			if err != nil {
				t.Fatal(err)
			}
			return wf
		}
	}
	t.Fatalf("no %s in %+v", path, c.Ops)
	return nil
}

func TestDefineWorkflow_StepsAreToldHowWorkMovesBetweenThem(t *testing.T) {
	wf := renderSteps(t, "draft", "critique", "edit")
	// The template's last step and the registry's answer step agree.
	if got := wf.AnswerSteps(); !reflect.DeepEqual(got, []string{"edit"}) {
		t.Fatalf("AnswerSteps = %v, want [edit]", got)
	}
	has := func(step, s string) bool { return strings.Contains(wf.Steps[step].Prompt, s) }
	for _, st := range []string{"draft", "critique"} {
		if !has(st, handoffSentence) || !has(st, reservedName) || has(st, answerSentence) {
			t.Errorf("%s: want the hand-off and the result.json reservation, not the answer instruction:\n%s", st, wf.Steps[st].Prompt)
		}
	}
	for _, st := range []string{"critique", "edit"} {
		if !has(st, earlierSentence) {
			t.Errorf("%s: no %q", st, earlierSentence)
		}
	}
	if has("draft", earlierSentence) {
		t.Error("the first step is told about earlier steps")
	}
	if !has("edit", answerSentence) || !has("edit", closingSentence) || has("edit", handoffSentence) {
		t.Errorf("edit: want the answer instruction and the closing sentence only:\n%s", wf.Steps["edit"].Prompt)
	}
}

// A one-step workflow is first and answer step at once: today's text.
func TestDefineWorkflow_OneStepKeepsTodaysText(t *testing.T) {
	wf := renderSteps(t, "only")
	p := wf.Steps["only"].Prompt
	for _, s := range []string{handoffSentence, earlierSentence, reservedName} {
		if strings.Contains(p, s) {
			t.Errorf("one-step prompt carries %q:\n%s", s, p)
		}
	}
	if !strings.Contains(p, answerSentence) || !strings.Contains(p, "Write no other output file.") {
		t.Errorf("one-step prompt lost today's answer text:\n%s", p)
	}
}

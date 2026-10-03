package registry

import (
	"reflect"
	"testing"
)

// Agent-administered design §18.10 (2026-10-03): which step writes a broker
// workflow's answer is decided once, here, so the step template and the task
// prompt cannot disagree. Before it, every step of a multi-step agent
// workflow was told its answer was result.json (task
// task_20261003001729_083c470d9167367c lost its hand-off).
func TestAnswerSteps_StepsWhoseSuccessorIsATerminal(t *testing.T) {
	linear := &Workflow{
		Entrypoint: "a",
		Steps: map[string]WorkflowStep{
			"a": {OnSuccess: "b"}, "b": {OnSuccess: "c"}, "c": {OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{"done": {}},
	}
	if got := linear.AnswerSteps(); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("linear: %v, want [c]", got)
	}
	branching := &Workflow{
		Entrypoint: "triage",
		Steps: map[string]WorkflowStep{
			"triage": {OnSuccess: "short"}, "short": {OnSuccess: "done"}, "long": {OnSuccess: "failed"},
		},
		Terminals: map[string]WorkflowTerminal{"done": {}, "failed": {}},
	}
	if got := branching.AnswerSteps(); !reflect.DeepEqual(got, []string{"long", "short"}) {
		t.Fatalf("branching: %v, want [long short]", got)
	}
	if got := (&Workflow{}).AnswerSteps(); len(got) != 0 {
		t.Fatalf("empty workflow: %v", got)
	}
}

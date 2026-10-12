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

// audit 2026-10-12, finding A: AnswerSteps counted only on_success, so a
// broker workflow whose answer step reaches done through a gate (an agent
// step's inline gates, or a standalone gate/approval step) had no answer
// step, and result returned egress_no_output for an answer it had written.
func TestAnswerSteps_GateRoutedAnswer(t *testing.T) {
	inline := &Workflow{
		Entrypoint: "draft",
		Steps: map[string]WorkflowStep{
			"draft":  {Type: "agent", OnSuccess: "review"},
			"review": {Type: "agent", Gates: []WorkflowGate{{Condition: "ok == true", Target: "done"}, {Condition: "ok == false", Target: "draft"}}},
		},
		Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	if got := inline.AnswerSteps(); !reflect.DeepEqual(got, []string{"review"}) {
		t.Fatalf("inline gates: %v, want [review]", got)
	}
	standalone := &Workflow{
		Entrypoint: "read",
		Steps: map[string]WorkflowStep{
			"read":    {Type: "agent", OnSuccess: "check"},
			"check":   {Type: "gate", Gates: []WorkflowGate{{Condition: "ok == true", Target: "signoff"}}, OnSuccess: "retry"},
			"signoff": {Type: "approval", OnSuccess: "done"},
			"retry":   {Type: "agent", OnSuccess: "done", OnFail: "failed"},
			"never":   {Type: "agent", OnSuccess: "read", OnFail: "failed"},
		},
		Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"}},
	}
	// read reaches done through the gate and the approval step; neither
	// pass-through step writes an answer; on_fail is not a success edge.
	if got := standalone.AnswerSteps(); !reflect.DeepEqual(got, []string{"read", "retry"}) {
		t.Fatalf("standalone gate: %v, want [read retry]", got)
	}
	// Any terminal counts, whatever its status, as it did for on_success
	// (review-20261012-87d6 F10): a gate to a FAILED terminal still makes
	// the step an answer step.
	toFailed := &Workflow{
		Entrypoint: "triage",
		Steps: map[string]WorkflowStep{
			"triage": {Type: "agent", Gates: []WorkflowGate{{Condition: "ok == false", Target: "failed"}, {Condition: "ok == true", Target: "work"}}},
			"work":   {Type: "agent", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"}},
	}
	if got := toFailed.AnswerSteps(); !reflect.DeepEqual(got, []string{"triage", "work"}) {
		t.Fatalf("gate to a failed terminal: %v, want [triage work]", got)
	}
}

// audit 2026-10-12, finding A: an on_outcome route to a terminal is a
// success edge too.
func TestAnswerSteps_OnOutcomeRoutedAnswer(t *testing.T) {
	wf := &Workflow{
		Entrypoint: "fetch",
		Steps: map[string]WorkflowStep{
			"fetch": {Type: "system", Handler: "x", OnOutcome: map[string]string{"empty": "done"}, OnSuccess: "sum"},
			"sum":   {Type: "agent", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	if got := wf.AnswerSteps(); !reflect.DeepEqual(got, []string{"fetch", "sum"}) {
		t.Fatalf("on_outcome: %v, want [fetch sum]", got)
	}
}

// audit 2026-10-12, finding A: a cycle of pass-through steps is walked once.
func TestAnswerSteps_PassThroughCycleTerminates(t *testing.T) {
	wf := &Workflow{
		Entrypoint: "a",
		Steps: map[string]WorkflowStep{
			"a":  {Type: "agent", OnSuccess: "g1"},
			"g1": {Type: "gate", Gates: []WorkflowGate{{Condition: "x", Target: "g2"}}},
			"g2": {Type: "gate", Gates: []WorkflowGate{{Condition: "x", Target: "g1"}}},
		},
		Terminals: map[string]WorkflowTerminal{"done": {}},
	}
	if got := wf.AnswerSteps(); len(got) != 0 {
		t.Fatalf("cycle with no terminal: %v, want none", got)
	}
}

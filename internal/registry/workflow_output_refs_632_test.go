package registry

import (
	"strings"
	"testing"
)

// GitHub #632. A ${outputs.<step>.<field>} reference that can NEVER resolve
// used to pass load and fail (prompt) or silently become "" (payload) on every
// run: references to step types that record no result, to call_project (whose
// only readable value is bookkeeping, not the callee's answer), and to the
// step itself (always absent once entering a step clears its entry).
//
// Found at: review (the #631 design review). Regression test here: the
// validator, at the level the defect is refused (load time).
//
// Design: https://docs.vornik.io,
// Amendment 2026-11-04, F3.

func refsWorkflow(steps map[string]WorkflowStep) *Workflow {
	all := map[string]WorkflowStep{
		"src_agent":    {Type: "agent", Role: "r", OnSuccess: "done"},
		"src_system":   {Type: "system", Handler: "h", OnSuccess: "done"},
		"src_plan":     {Type: "plan", Role: "lead", OnSuccess: "done"},
		"src_spawn":    {Type: "spawn_project", Template: "t", OnSuccess: "done"},
		"src_a2a":      {Type: "a2a_call", AgentURL: "https://partner.example", Prompt: "x", OnSuccess: "done"},
		"src_call":     {Type: "call_project", TargetProject: "p2", OnSuccess: "done"},
		"src_gate":     {Type: "gate", OnSuccess: "done"},
		"src_approval": {Type: "approval", OnSuccess: "done"},
		"src_parallel": {Type: "parallel", OnSuccess: "done"},
	}
	for k, v := range steps {
		all[k] = v
	}
	return &Workflow{ID: "wf-632", Entrypoint: "src_agent", Steps: all,
		Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}}}
}

func TestOutputRefs632_RecordingTypesAreAccepted(t *testing.T) {
	for _, typ := range RecordingStepTypes {
		src := map[string]string{"agent": "src_agent", "system": "src_system", "plan": "src_plan",
			"spawn_project": "src_spawn", "a2a_call": "src_a2a", "call_project": "src_call"}[typ]
		if src == "" {
			t.Fatalf("RecordingStepTypes has %q, which this test has no source step for: add one", typ)
		}
		w := refsWorkflow(map[string]WorkflowStep{
			"reader": {Type: "agent", Role: "r", Prompt: "v=${outputs." + src + ".x}", OnSuccess: "done"},
		})
		if err := w.validatePromptOutputRefs("wf.md"); err != nil {
			t.Errorf("a prompt reference to a %s step must load: %v", typ, err)
		}
	}
}

func TestOutputRefs632_NeverResolvableReferencesAreRefused(t *testing.T) {
	cases := []struct{ ref, why string }{
		{"${outputs.src_gate.x}", "gate step, which records no result"},
		{"${outputs.src_approval.x}", "approval step, which records no result"},
		{"${outputs.src_parallel.x}", "parallel step, which records no result"},
		{"${outputs.reader.x}", "a step cannot read its own output"},
	}
	for _, c := range cases {
		for _, channel := range []string{"prompt", "payload"} {
			reader := WorkflowStep{Type: "agent", Role: "r", Prompt: "v=" + c.ref, OnSuccess: "done"}
			if channel == "payload" {
				reader = WorkflowStep{Type: "call_project", TargetProject: "p3",
					Payload: map[string]any{"nested": []any{map[string]any{"v": "a " + c.ref}}}, OnSuccess: "done"}
			}
			w := refsWorkflow(map[string]WorkflowStep{"reader": reader})
			err := w.validatePromptOutputRefs("wf.md")
			if err == nil {
				t.Errorf("%s %s: must be refused at load", channel, c.ref)
				continue
			}
			if !strings.Contains(err.Error(), c.why) || !strings.Contains(err.Error(), "output reference") {
				t.Errorf("%s %s: the refusal must say %q, got: %v", channel, c.ref, c.why, err)
			}
		}
	}
}

func TestOutputRefs632_PayloadReferenceToAMissingStepIsRefused(t *testing.T) {
	w := refsWorkflow(map[string]WorkflowStep{
		"reader": {Type: "spawn_project", Template: "t",
			Params: map[string]any{"name": "${outputs.no_such_step.x}"}, OnSuccess: "done"},
	})
	err := w.validatePromptOutputRefs("wf.md")
	if err == nil || !strings.Contains(err.Error(), "does not define") {
		t.Fatalf("a payload reference to a missing step must be refused, got: %v", err)
	}
}

// Only an agent step's prompt is interpolated: a reference in an a2a_call
// prompt would reach the partner as literal text.
func TestOutputRefs632_ReferenceInANonAgentPromptIsRefused(t *testing.T) {
	w := refsWorkflow(map[string]WorkflowStep{
		"reader": {Type: "a2a_call", AgentURL: "https://partner.example",
			Prompt: "summarise ${outputs.src_agent.message}", OnSuccess: "done"},
	})
	err := w.validatePromptOutputRefs("wf.md")
	if err == nil || !strings.Contains(err.Error(), "only an agent step's prompt resolves") {
		t.Fatalf("a reference in an a2a_call prompt must be refused, got: %v", err)
	}
}

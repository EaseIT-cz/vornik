package configassist

// GitHub #892 (config-assistant design §6.3.4d): healing asked the assistant
// to repair exactly workflows/<trigger workflow>.md, but its read and write
// scope held only the project's default and autonomy workflows. On the #809
// bench every trigger on dev-pipeline (the bench project defaults to
// simple-workflow) ended NO_CHANGE: "the file does not exist". Found at:
// e2e-lane (the #809 bench daemon); tested at the engine, where the scope is
// built.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

func TestProposeForHealing_ANonDefaultWorkflowIsInScope_892(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["workflows/w2.md"]`,
			call("c1", "file_edit", map[string]any{"path": "workflows/w2.md", "old_string": "timeout: \"10m\"", "new_string": "timeout: \"15m\""})),
		textResp("glm-5.2:cloud", "Adjusted the workflow."),
	}
	f.scriptJudge("pass", "timeout 15m")
	res, err := f.engine.ProposeForHealing(context.Background(), HealingRequest{ProjectID: "assistant", WorkflowID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Refusal != nil || res.Proposal == nil {
		t.Fatalf("a healing repair of a non-default workflow must be proposed: %+v %+v", res.Refusal, res.Proposal)
	}
	if !strings.Contains(res.Proposal.ApplyOps, "w2.md") {
		t.Fatalf("the proposal must edit w2.md: %s", res.Proposal.ApplyOps)
	}
}

// Only the healing request's own workflow is admitted: an operator request in
// the same project still cannot write w2.
func TestPropose_OperatorScopeUnchanged_892(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["workflows/w2.md"]`,
			call("c1", "file_edit", map[string]any{"path": "workflows/w2.md", "old_string": "timeout: \"10m\"", "new_string": "timeout: \"15m\""})),
		textResp("glm-5.2:cloud", "Adjusted the workflow."),
	}
	f.scriptJudge("pass", "timeout 15m")
	res, _ := f.engine.Propose(context.Background(), operatorReq("raise w2's timeout"))
	if res == nil || res.Proposal != nil && strings.Contains(res.Proposal.ApplyOps, "w2.md") {
		t.Fatalf("an operator request must not gain w2: %+v", res)
	}
}

// A workflow the registry does not know is never admitted.
func TestProposeForHealing_UnknownWorkflowRefused_892(t *testing.T) {
	f := newFixture(t)
	_, err := f.engine.ProposeForHealing(context.Background(), HealingRequest{ProjectID: "assistant", WorkflowID: "../projects/other"})
	if err == nil || !strings.Contains(err.Error(), "workflow") {
		t.Fatalf("an unknown or path-like workflow must be refused, got %v", err)
	}
}

// Exactly the named workflow joins: a healing request for w2 that also edits
// w3 never carries w3 (review 129d).
func TestProposeForHealing_OnlyTheNamedWorkflowJoins_892(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["workflows/w2.md", "workflows/w3.md"]`,
			call("c1", "file_edit", map[string]any{"path": "workflows/w2.md", "old_string": "timeout: \"10m\"", "new_string": "timeout: \"15m\""}),
			call("c2", "file_edit", map[string]any{"path": "workflows/w3.md", "old_string": "timeout: \"10m\"", "new_string": "timeout: \"15m\""})),
		textResp("glm-5.2:cloud", "Adjusted both."),
	}
	f.scriptJudge("pass", "timeout 15m")
	res, err := f.engine.ProposeForHealing(context.Background(), HealingRequest{ProjectID: "assistant", WorkflowID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	// The w3 edit must not land: either the request is refused, or the
	// proposal carries w2 alone (the tool loop refuses a file outside scope).
	if res.Proposal != nil && strings.Contains(res.Proposal.ApplyOps, "w3.md") {
		t.Fatalf("w3 is outside the healing request's scope but reached the proposal: %s", res.Proposal.ApplyOps)
	}
	if res.Proposal != nil && !strings.Contains(res.Proposal.ApplyOps, "w2.md") {
		t.Fatalf("the proposal must still carry the named workflow: %s", res.Proposal.ApplyOps)
	}
}

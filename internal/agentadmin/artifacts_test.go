package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

// GitHub #318: whole-file outputs widen approved egress, even with unchanged JSON.
func TestDefineWorkflowArtifactReach(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "finance", Roles: []RoleInput{{Name: "writer", Instructions: "Write.", Tools: []string{"file_write"}}}}))
	input := map[string]any{"project": "finance", "slug": "spend", "steps": []StepInput{{Name: "write", Role: "writer", Instructions: "Produce a report."}}, "inputs": json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`), "egress": egress1()}
	first := tr.render(VerbDefineWorkflow, input)
	tr.mustClass(first, Widening)
	tr.apply(first)
	input["artifacts"] = []string{"rendered.pdf", "report.md"}
	added := tr.render(VerbDefineWorkflow, input)
	tr.mustClass(added, Widening)
	if !strings.Contains(added.Sentence, "whole-file") || !strings.Contains(added.Sentence, "rendered.pdf") {
		t.Fatalf("approval omits file reach: %s", added.Sentence)
	}
	tr.apply(added)
	tr.mustClass(tr.render(VerbDefineWorkflow, input), Inert)
	input["artifacts"] = []string{"../secret"}
	tr.mustClass(tr.render(VerbDefineWorkflow, input), Refused)
}

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/registry"
)

// The broker step was told to write "exactly the approved egress shape" and
// never shown it, so it guessed the field names: task
// task_20261002234008_bdb3d14c17e1af7b wrote edge_cases[].why_it_fails where
// the approved schema says cases[].why, and ended egress_schema with no
// output (agent-administered Vornik design §18.1b). The prompt now carries the
// output file and the schema, byte-identical to its canonical JSON.
func TestRenderBrokerPrompt_CarriesTheEgressSchemaVerbatim(t *testing.T) {
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"cases"},
		"properties": map[string]any{"cases": map[string]any{
			"type": "array", "maxItems": float64(20),
			"items": map[string]any{
				"type": "object", "additionalProperties": false, "required": []any{"situation", "why"},
				"properties": map[string]any{
					"situation": map[string]any{"type": "string", "maxLength": float64(500)},
					"why":       map[string]any{"type": "string", "maxLength": float64(800)},
				},
			},
		}},
	}
	wf := &registry.Workflow{ID: "ns--p--edgecases", Broker: &registry.WorkflowBroker{
		InputSchema: map[string]any{"type": "object"},
		Egress:      registry.BrokerEgress{Output: "result.json", Schema: schema},
	}}
	prompt, err := renderBrokerPrompt(wf, map[string]any{})
	require.NoError(t, err)

	raw, err := json.Marshal(schema)
	require.NoError(t, err)
	canon, err := approval.Canonical(raw)
	require.NoError(t, err)
	require.Contains(t, prompt, string(canon), "the prompt must carry the approved egress schema byte for byte")
	require.Contains(t, prompt, "artifacts/out/result.json")
	for _, field := range []string{`"cases"`, `"situation"`, `"why"`, `"maxItems":20`} {
		require.True(t, strings.Contains(prompt, field), "missing %s", field)
	}
}

// Design §18.10 (2026-10-03): the task prompt is one string for every step,
// and it told every step "Your answer is the file artifacts/out/result.json",
// so an intermediate step read the final answer as its own task. It now
// names the step that answers (from registry.AnswerSteps) and tells the
// others not to write it. Review 0575 F7: the old sentence is gone.
func TestRenderBrokerPrompt_NamesTheStepThatAnswers(t *testing.T) {
	egress := registry.BrokerEgress{Output: "result.json", Schema: map[string]any{"type": "object"}}
	linear := &registry.Workflow{ID: "ns--p--gaps", Entrypoint: "draft",
		Steps: map[string]registry.WorkflowStep{
			"draft": {OnSuccess: "critique"}, "critique": {OnSuccess: "edit"}, "edit": {OnSuccess: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {}},
		Broker:    &registry.WorkflowBroker{InputSchema: map[string]any{"type": "object"}, Egress: egress},
	}
	prompt, err := renderBrokerPrompt(linear, map[string]any{})
	require.NoError(t, err)
	require.Contains(t, prompt, "written by its last step (`edit`)")
	require.Contains(t, prompt, "If your step's instructions do not say you are that step, do not write it.")
	require.NotContains(t, prompt, "Your answer is the file")

	branching := &registry.Workflow{ID: "ns--p--triage", Entrypoint: "triage",
		Steps: map[string]registry.WorkflowStep{
			"triage": {OnSuccess: "short"}, "short": {OnSuccess: "done"}, "long": {OnSuccess: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {}},
		Broker:    &registry.WorkflowBroker{InputSchema: map[string]any{"type": "object"}, Egress: egress},
	}
	prompt, err = renderBrokerPrompt(branching, map[string]any{})
	require.NoError(t, err)
	require.Contains(t, prompt, "written by whichever of `long`, `short` ends the run")
}

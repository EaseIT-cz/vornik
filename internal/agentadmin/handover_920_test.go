package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

// GitHub #920 (agent-administered design part 24 §24.3 B1): define_workflow's
// hand_over.

func handOverTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.project("research")
	tr.project("venues")
	// The target: enrich, in venues, taking the venues document.
	b, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"venues": map[string]any{"type": "string", "x-untrusted-document": map[string]any{"max_bytes": 65536, "media_type": "text/markdown"}},
	}})
	tr.apply(tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "venues", Slug: "enrich", Purpose: "enrich venues",
		Steps: []StepInput{{Name: "enrich", Role: "worker", Instructions: "Enrich the venues."}}, Inputs: b, Egress: egress1()}))
	return tr
}

func scoutWF(tr *tree, h *HandOverInput) Change {
	tr.t.Helper()
	return tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "research", Slug: "scout", Purpose: "find venues",
		Steps:    []StepInput{{Name: "scout", Role: "worker", Instructions: "Find new venues."}},
		Inputs:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
		Egress:   egress1(),
		HandOver: h})
}

func TestDefineWorkflow_HandOver_920(t *testing.T) {
	tr := handOverTree(t)
	h := &HandOverInput{Project: "venues", Workflow: "enrich", Documents: map[string]string{"venues": "artifacts/out/scout.md"}}
	c := scoutWF(tr, h)
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Ops[0].Content, `hand_over: {"documents":{"venues":"artifacts/out/scout.md"},"workflow":"hermes--venues--enrich"}`) {
		t.Fatalf("rendered:\n%s", c.Ops[0].Content)
	}
	if !strings.Contains(c.Sentence, `When it finishes (this project: no connections), it starts the workflow "hermes--venues--enrich" (its project: no connections) and hands it venues from artifacts/out/scout.md.`) {
		t.Fatalf("sentence: %s", c.Sentence)
	}
	tr.apply(c)
	tr.mustClass(scoutWF(tr, h), Inert)
	// The hand-over is reach: removing it, or changing its document, widens.
	tr.mustClass(scoutWF(tr, nil), Widening)
	tr.mustClass(scoutWF(tr, &HandOverInput{Project: "venues", Workflow: "enrich", Documents: map[string]string{"venues": "artifacts/out/other.md"}}), Widening)
}

func TestDefineWorkflow_HandOverRefusals_920(t *testing.T) {
	tr := handOverTree(t)
	for name, c := range map[string]struct {
		h    *HandOverInput
		says string
	}{
		"unknown target":      {&HandOverInput{Project: "venues", Workflow: "nope"}, "not a loaded workflow"},
		"undeclared document": {&HandOverInput{Project: "venues", Workflow: "enrich", Documents: map[string]string{"menus": "artifacts/out/scout.md"}}, "declares no document input named"},
		"input not in schema": {&HandOverInput{Project: "venues", Workflow: "enrich", Inputs: json.RawMessage(`{"city":"Prague"}`)}, "input schema"},
		"itself":              {&HandOverInput{Project: "research", Workflow: "scout"}, "itself"},
	} {
		got := scoutWF(tr, c.h)
		if got.Class != Refused || !strings.Contains(got.Reason, c.says) {
			t.Errorf("%s: %v %q", name, got.Class, got.Reason)
		}
	}
}

// §24.3 B1: the cycle check walks the whole namespace, so a loop is refused
// whichever side is defined last.
func TestDefineWorkflow_HandOverCycle_920(t *testing.T) {
	tr := handOverTree(t)
	tr.apply(scoutWF(tr, &HandOverInput{Project: "venues", Workflow: "enrich", Documents: map[string]string{"venues": "artifacts/out/scout.md"}}))
	back := tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "venues", Slug: "enrich", Purpose: "enrich venues",
		Steps:    []StepInput{{Name: "enrich", Role: "worker", Instructions: "Enrich the venues."}},
		Inputs:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"venues":{"type":"string","x-untrusted-document":{"max_bytes":65536,"media_type":"text/markdown"}}}}`),
		Egress:   egress1(),
		HandOver: &HandOverInput{Project: "research", Workflow: "scout"}})
	if back.Class != Refused || !strings.Contains(back.Reason, "loop") {
		t.Fatalf("enrich -> scout after scout -> enrich: %v %q", back.Class, back.Reason)
	}
}

func TestDefineWorkflow_HandOverCycleOtherDirection_920(t *testing.T) {
	tr := handOverTree(t)
	tr.apply(scoutWF(tr, nil))
	tr.apply(tr.render(VerbDefineWorkflow, DefineWorkflowInput{Project: "venues", Slug: "enrich", Purpose: "enrich venues",
		Steps:    []StepInput{{Name: "enrich", Role: "worker", Instructions: "Enrich the venues."}},
		Inputs:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"venues":{"type":"string","x-untrusted-document":{"max_bytes":65536,"media_type":"text/markdown"}}}}`),
		Egress:   egress1(),
		HandOver: &HandOverInput{Project: "research", Workflow: "scout"}}))
	got := scoutWF(tr, &HandOverInput{Project: "venues", Workflow: "enrich", Documents: map[string]string{"venues": "artifacts/out/scout.md"}})
	if got.Class != Refused || !strings.Contains(got.Reason, "loop") {
		t.Fatalf("scout -> enrich after enrich -> scout: %v %q", got.Class, got.Reason)
	}
}

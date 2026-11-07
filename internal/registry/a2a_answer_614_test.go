package registry

import (
	"strings"
	"testing"
)

// #614 C5: the loader refuses an a2a answer policy it could not enforce
// as written. Before the change the block did not exist.
func TestWorkflowValidate_A2AAnswerPolicy_614(t *testing.T) {
	base := func(a *WorkflowA2AAnswer) *Workflow {
		return &Workflow{
			ID: "w", Entrypoint: "s",
			Steps:     map[string]WorkflowStep{"s": {Type: "agent", Role: "r", OnSuccess: "done"}},
			Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
			A2A:       WorkflowA2A{Publish: true, Answer: a},
		}
	}
	ok := func() *WorkflowA2AAnswer {
		return &WorkflowA2AAnswer{Artifact: "result.json", Fields: []string{"answer", "citations"}, MaxBytes: 8192}
	}
	if err := base(ok()).Validate("w.md"); err != nil {
		t.Fatalf("a well-formed policy must load: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(a *WorkflowA2AAnswer)
		want string
	}{
		{"no artifact", func(a *WorkflowA2AAnswer) { a.Artifact = "" }, "artifact"},
		{"a path, not a name", func(a *WorkflowA2AAnswer) { a.Artifact = "out/result.json" }, "artifact"},
		{"not json", func(a *WorkflowA2AAnswer) { a.Artifact = "result.md" }, "artifact"},
		{"no fields", func(a *WorkflowA2AAnswer) { a.Fields = nil }, "fields"},
		{"an empty field", func(a *WorkflowA2AAnswer) { a.Fields = []string{"answer", ""} }, "fields"},
		{"a duplicate field", func(a *WorkflowA2AAnswer) { a.Fields = []string{"answer", "answer"} }, "fields"},
		{"maxBytes unset", func(a *WorkflowA2AAnswer) { a.MaxBytes = 0 }, "maxBytes"},
		{"maxBytes over the 64 KiB read bound", func(a *WorkflowA2AAnswer) { a.MaxBytes = 64*1024 + 1 }, "maxBytes"},
		{"unknown codeBlocks", func(a *WorkflowA2AAnswer) { a.CodeBlocks = "allow" }, "codeBlocks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := ok()
			tc.mut(a)
			err := base(a).Validate("w.md")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %q, got %v", tc.want, err)
			}
		})
	}
	for _, cb := range []string{"", "refuse", "strip"} {
		a := ok()
		a.CodeBlocks = cb
		if err := base(a).Validate("w.md"); err != nil {
			t.Fatalf("codeBlocks %q must load: %v", cb, err)
		}
	}
}

func TestParseWorkflow_A2AAnswerPolicy_614(t *testing.T) {
	md := `---
workflowId: "w"
entrypoint: "s"
a2a:
  publish: true
  answer:
    artifact: result.json
    fields: [answer, citations]
    maxBytes: 8192
    codeBlocks: strip
steps:
  s:
    type: agent
    role: r
    on_success: done
terminals:
  done:
    status: "COMPLETED"
---

## Prompts

### s

Answer.
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "w.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := wf.A2A.Answer
	if a == nil || a.Artifact != "result.json" || len(a.Fields) != 2 || a.MaxBytes != 8192 || a.CodeBlocks != "strip" {
		t.Fatalf("the answer block did not round-trip: %+v", a)
	}
}

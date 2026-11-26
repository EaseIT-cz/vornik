package agentadmin

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Hermes's hand-over request (2026-11-25, approved three times, 16:25,
// 16:26 and 16:27) mapped "md" to the bare name "enrich.md", the form the
// workflow's own artifacts list uses. define_workflow accepted it; the
// registry refused it at apply ("must be a file directly under
// artifacts/out/"), so each approval was reverted after the operator gave it.
//
// Found at: production (the phone's "Could not be applied").
// Regression test at: unit (the verb's output run through the registry's
// own Workflow.Validate, the check that refused it at apply).
// Lower because: the apply path is the same Validate; reaching it here
// needs no phone, daemon or reload.

func validates(t *testing.T, c Change) error {
	t.Helper()
	for _, op := range c.Ops {
		if !strings.HasSuffix(op.Path, ".md") || !strings.Contains(op.Path, "workflows/") {
			continue
		}
		wf, err := registry.ParseWorkflowMarkdown([]byte(op.Content), op.Path)
		if err != nil {
			return err
		}
		if err := wf.Validate(op.Path); err != nil {
			return err
		}
	}
	return nil
}

func TestDefineWorkflow_HandOverBareNameIsCompleted(t *testing.T) {
	tr := handOverTree(t)
	c := scoutWF(tr, &HandOverInput{Project: "venues", Workflow: "enrich", Documents: map[string]string{"venues": "scout.md"}})
	if c.Class == Refused {
		t.Fatalf("a bare output name is refused: %s", c.Reason)
	}
	if !strings.Contains(c.Ops[0].Content, `"venues":"artifacts/out/scout.md"`) {
		t.Fatalf("the bare name is not completed to artifacts/out/:\n%s", c.Ops[0].Content)
	}
	if !strings.Contains(c.Sentence, "hands it venues from artifacts/out/scout.md") {
		t.Fatalf("the phone must show the path that will be used: %s", c.Sentence)
	}
	if err := validates(t, c); err != nil {
		t.Fatalf("what the verb renders must pass the registry's own check: %v", err)
	}
}

func TestDefineWorkflow_HandOverBadPathIsRefusedBeforeApproval(t *testing.T) {
	tr := handOverTree(t)
	for _, p := range []string{"artifacts/out/sub/scout.md", "../scout.md", "/etc/passwd", "artifacts/in/scout.md", "", ".hidden"} {
		c := scoutWF(tr, &HandOverInput{Project: "venues", Workflow: "enrich", Documents: map[string]string{"venues": p}})
		if c.Class != Refused || !strings.Contains(c.Reason, "artifacts/out/") {
			t.Errorf("%q: %v %q, want refused naming artifacts/out/", p, c.Class, c.Reason)
		}
	}
}

package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

// Broker design §18.4 / §18.6 (a document input, GREEN at review 7514): a
// document bound is reach (agent-administered design §7.6 as amended). A
// workflow approved with a document runs; once its declaration changes on
// disk it is refused REACH_NOT_APPROVED until approved again. Control:
// SignatureOf's Documents, judged by verifyAgentReach.
func TestAgentAdmin_DocumentDeclarationIsApprovedReach(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "docs", Purpose: "Design reviews"})
	in := agentadmin.DefineWorkflowInput{Project: "docs", Slug: "review",
		Steps:  []agentadmin.StepInput{{Name: "read", Role: "worker", Instructions: "Review the design."}},
		Inputs: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["design"],"properties":{"design":{"type":"string","x-untrusted-document":{"max_bytes":65536,"media_type":"text/markdown"}}}}`),
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"findings":{"type":"array","maxItems":10,"items":{"type":"string","maxLength":300}}}}`)}
	res := f.do(agentadmin.VerbDefineWorkflow, in)
	if res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("a document workflow did not ask for approval: %+v", res)
	}
	f.approve(res)
	if err := f.c.verifyAgentReach(ctx, "hermes--docs", "hermes--docs--review"); err != nil {
		t.Fatalf("the approved document workflow was refused: %v", err)
	}

	// A hand edit raises the bound. The file still loads; the reach no
	// longer matches.
	path := filepath.Join(f.cfgDir, "configs", "workflows", "hermes--docs--review.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), `"max_bytes":65536`, `"max_bytes":131072`, 1)
	if edited == string(b) {
		t.Fatalf("the edit did not apply; the rendered workflow changed shape:\n%s", b)
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.c.ConfigReloader.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--docs", "hermes--docs--review"); err == nil || !strings.Contains(err.Error(), "changed what it returns or can reach") {
		t.Fatalf("a workflow whose document bound changed was allowed to run: %v", err)
	}
}

// Broker design §18.5 / §18.7 F5: describe_installation states the caps and
// that each step that reads the document pays for it.
func TestAgentAdmin_DescribeStatesTheDocumentInput(t *testing.T) {
	f := newAgentAdminFixture(t)
	caps, err := f.svc.Describe(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"262,144", "524,288", "artifacts/in/", "step that reads it pays"} {
		if !strings.Contains(caps.Documents, want) {
			t.Errorf("documents statement lacks %q: %q", want, caps.Documents)
		}
	}
	found := false
	for _, r := range caps.InputRules {
		found = found || r.ID == "document"
	}
	if !found {
		t.Error("the document rule is not among the published input rules")
	}
}

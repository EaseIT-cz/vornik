package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// Design §18.3 (2026-10-02): catalog hid the agent's own workflows; the
// agent could delegate claudecode--docreview--review but catalog never
// listed it. ApprovedWorkflows is what catalog lists: every approved
// workflow of the namespace, never a pending or placeholder one, never
// another namespace's. Control: agentAdminService.ApprovedWorkflows.
func TestAgentAdmin_ApprovedWorkflowsAreTheNamespacesApprovedOnes(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	egress := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)
	define := func(project, slug string) agentadmin.Result {
		return f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: project, Slug: slug,
			Steps: []agentadmin.StepInput{{Name: "s", Role: "worker", Instructions: "x"}}, Egress: egress})
	}
	for _, p := range []string{"finance", "travel"} {
		f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: p, Purpose: p})
	}
	f.approve(define("finance", "spend"))
	f.approve(define("travel", "plan"))
	if res := define("finance", "draft"); res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("define draft: %+v", res)
	}

	got, err := f.svc.ApprovedWorkflows(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hermes--finance--spend", "hermes--travel--plan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("approved workflows = %v, want %v (pending and placeholder workflows excluded)", got, want)
	}

	// A namespace whose name is a prefix of this one sees none of them.
	her := &persistence.APIKey{ID: "akey_her", ClientKind: "codex", AgentAdmin: true, AgentNamespace: "her"}
	if other, err := f.svc.ApprovedWorkflows(ctx, her); err != nil || len(other) != 0 {
		t.Fatalf("namespace her sees %v (%v)", other, err)
	}
}

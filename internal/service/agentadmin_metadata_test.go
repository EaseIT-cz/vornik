package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// Issue #67 (2026-10-06): project purpose could not be edited after creation.
// Exercise the namespace admin renderer, durable apply/reload and list output.
func TestAgentAdmin_UpdateProjectMetadata(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "news", Purpose: "Original purpose"})
	pid := "hermes--news"
	ctx := context.Background()
	// Existing grant rows are the authority for reach; a metadata change may
	// neither withdraw nor replace them (implementation review 3b9d F3).
	if err := f.svc.grants.UpsertWorkflowReach(ctx, persistence.AgentWorkflowApproval{Namespace: "hermes", ProjectID: pid, WorkflowID: pid + "--start", ReachHash: "unchanged", ApprovedByDevice: f.device.ID, ApprovedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.grants.UpsertIntegration(ctx, persistence.AgentIntegrationApproval{Namespace: "hermes", ProjectID: pid, Integration: "reader", Kind: "mcp", URL: "https://example.com/mcp", ReadTools: []string{"read"}, ApprovedByDevice: f.device.ID, ApprovedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	reachBefore, err := f.svc.grants.ListWorkflowReach(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	integrationsBefore, err := f.svc.grants.ListIntegrations(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.cfgDir, "configs", "projects", pid+".yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before = append(before, []byte("\n# preserve operator settings\nretention:\n  tasks_days: 17\n")...)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.do("update_project", map[string]any{"project": "news", "purpose": "Global news", "display_name": "Hermes: Global News"})
	if res.Effect != agentadmin.EffectApplied {
		t.Fatalf("metadata not applied: %+v", res)
	}
	p := f.c.Registry.GetProject(pid)
	if p == nil || p.Description != "Global news" || p.DisplayName != "Hermes: Global News" {
		t.Fatalf("loaded project: %+v", p)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "preserve operator settings") || !strings.Contains(string(after), "tasks_days: 17") {
		t.Fatalf("sibling YAML lost: %s", after)
	}
	if p.ID != pid || p.Budget.MonthlyHardUSD != 2 {
		t.Fatalf("routing/spend changed: %+v", p)
	}
	setup, err := f.svc.ListSetup(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(setup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"display_name":"Hermes: Global News"`) || !strings.Contains(string(raw), `"purpose":"Global news"`) {
		t.Fatalf("metadata not listed: %s", raw)
	}
	res = f.do("update_project", map[string]any{"project": "news", "purpose": "Updated scope"})
	if res.Effect != agentadmin.EffectApplied {
		t.Fatalf("partial update: %+v", res)
	}
	if p = f.c.Registry.GetProject(pid); p.Description != "Updated scope" || p.DisplayName != "Hermes: Global News" {
		t.Fatalf("partial update reset label: %+v", p)
	}
	if res = f.do("update_project", map[string]any{"project": "other--news", "purpose": "Cross namespace"}); res.Effect != agentadmin.EffectRefused {
		t.Fatalf("cross namespace: %+v", res)
	}
	reachAfter, err := f.svc.grants.ListWorkflowReach(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	integrationsAfter, err := f.svc.grants.ListIntegrations(ctx, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reachBefore, reachAfter) || !reflect.DeepEqual(integrationsBefore, integrationsAfter) {
		t.Fatal("metadata altered workflow/integration approvals")
	}
}

// Issue #67: a file changed after rendering must be preserved and refused by
// the durable apply read-set gate, not overwritten with the metadata snapshot.
func TestAgentAdmin_UpdateProjectMetadataRefusesDrift(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "news", Purpose: "Original"})
	ctx := context.Background()
	st, err := f.svc.loadState(ctx, "hermes", f.key.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := f.svc.renderer.Render(st, agentadmin.VerbUpdateProject, json.RawMessage(`{"project":"news","purpose":"Updated"}`))
	if err != nil || ch.Class != agentadmin.Inert {
		t.Fatalf("render: %+v %v", ch, err)
	}
	proposal, err := f.svc.fileProposal(ctx, "agent", f.key.ID, ch)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.cfgDir, "configs", "projects", "hermes--news.yaml")
	edited := append(append([]byte{}, st.ProjectYAML["projects/hermes--news.yaml"]...), []byte("\n# concurrent operator edit\n")...)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.applyInert(ctx, proposal, ch)
	if err != nil || res.Effect != agentadmin.EffectRefused || !strings.Contains(res.Reason, "changed") {
		t.Fatalf("drift accepted: %+v %v", res, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(edited) {
		t.Fatal("concurrent edit overwritten")
	}
}

// Issue #71: agents discover the same renderer capability they can grant.
func TestAgentAdmin_DescribeOffersDocumentRenderer(t *testing.T) {
	f := newAgentAdminFixture(t)
	v, err := f.svc.Describe(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range v.RoleBuiltins {
		if tool == "document_render" {
			found = true
		}
		if tool == "run_shell" {
			t.Fatal("shell capability advertised")
		}
	}
	if !found {
		t.Fatal("document renderer absent from describe_installation")
	}
}

package repotest

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunAgentGrantSuite pins the agent approval tables on both drivers
// (agent-administered Vornik design §7; plan P3.4). Namespaces and IDs are
// run-unique because the Postgres lane's database is shared.
func RunAgentGrantSuite(t *testing.T, repo persistence.AgentGrantRepository) {
	t.Helper()
	suffix := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(uniqueID("g")))
	h := &grantHarness{repo: repo, ctx: context.Background(), now: time.Now().UTC().Truncate(time.Second),
		ns: "g" + uniqueTail(suffix), other: "h" + uniqueTail(suffix)}
	h.project = h.ns + "--fin"
	t.Run("MissContract", func(t *testing.T) { grantMissContract(t, h) })
	t.Run("Integration_grant_remove_regrant", func(t *testing.T) { grantIntegration(t, h) })
	t.Run("Workflow_reach", func(t *testing.T) { grantWorkflowReach(t, h) })
	t.Run("Ceiling", func(t *testing.T) { grantCeiling(t, h) })
}

type grantHarness struct {
	repo      persistence.AgentGrantRepository
	ctx       context.Context
	now       time.Time
	ns, other string
	project   string
}

func grantMissContract(t *testing.T, h *grantHarness) {
	AssertMiss(t, "AgentGrantRepository.GetIntegration", func() (*persistence.AgentIntegrationApproval, error) {
		return h.repo.GetIntegration(h.ctx, h.project, "absent")
	})
	AssertMiss(t, "AgentGrantRepository.GetWorkflowReach", func() (*persistence.AgentWorkflowApproval, error) {
		return h.repo.GetWorkflowReach(h.ctx, h.ns+"--absent--wf")
	})
	AssertMiss(t, "AgentGrantRepository.GetCeiling", func() (*persistence.AgentNamespaceBudget, error) {
		return h.repo.GetCeiling(h.ctx, h.ns)
	})
}

func grantIntegration(t *testing.T, h *grantHarness) {
	a := persistence.AgentIntegrationApproval{Namespace: h.ns, ProjectID: h.project, Integration: "fio", Kind: "mcp",
		URL: "https://fio.example/mcp", ReadTools: []string{"balance", "list"}, ApprovedByDevice: "dev_a", ApprovedAt: h.now}
	if err := h.repo.UpsertIntegration(h.ctx, a); err != nil {
		t.Fatal(err)
	}
	other := a
	other.Namespace, other.ProjectID = h.other, h.other+"--fin"
	if err := h.repo.UpsertIntegration(h.ctx, other); err != nil {
		t.Fatal(err)
	}
	got, err := h.repo.GetIntegration(h.ctx, h.project, "fio")
	if err != nil || !reflect.DeepEqual(got.ReadTools, []string{"balance", "list"}) || len(got.WriteTools) != 0 ||
		got.ReadPending || got.RemovedAt != nil || !got.ApprovedAt.Equal(h.now) || got.Kind != "mcp" {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	list, err := h.repo.ListIntegrations(h.ctx, h.ns)
	if err != nil || len(list) != 1 || list[0].ProjectID != h.project {
		t.Fatalf("ListIntegrations leaks or misses: %+v, %v", list, err)
	}
	if err := h.repo.MarkIntegrationRemoved(h.ctx, h.project, "fio", h.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = h.repo.GetIntegration(h.ctx, h.project, "fio")
	if got.RemovedAt == nil || !reflect.DeepEqual(got.ReadTools, []string{"balance", "list"}) {
		t.Fatalf("removal must keep the history: %+v", got)
	}
	a.ReadTools, a.ReadPending = nil, true
	if err := h.repo.UpsertIntegration(h.ctx, a); err != nil {
		t.Fatal(err)
	}
	got, _ = h.repo.GetIntegration(h.ctx, h.project, "fio")
	if got.RemovedAt != nil || !got.ReadPending || len(got.ReadTools) != 0 {
		t.Fatalf("a re-grant must clear removed_at and replace the sets: %+v", got)
	}
}

func grantWorkflowReach(t *testing.T, h *grantHarness) {
	wf := h.project + "--spend"
	a := persistence.AgentWorkflowApproval{Namespace: h.ns, WorkflowID: wf, ProjectID: h.project, ReachHash: "h1", ApprovedByDevice: "dev_a", ApprovedAt: h.now}
	if err := h.repo.UpsertWorkflowReach(h.ctx, a); err != nil {
		t.Fatal(err)
	}
	a.ReachHash = "h2"
	if err := h.repo.UpsertWorkflowReach(h.ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := h.repo.GetWorkflowReach(h.ctx, wf)
	if err != nil || got.ReachHash != "h2" || got.ProjectID != h.project {
		t.Fatalf("reach: %+v, %v", got, err)
	}
	if list, _ := h.repo.ListWorkflowReach(h.ctx, h.ns); len(list) != 1 {
		t.Fatalf("ListWorkflowReach = %d rows", len(list))
	}
	if err := h.repo.DeleteWorkflowReach(h.ctx, wf); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.GetWorkflowReach(h.ctx, wf); err == nil {
		t.Fatal("the reach survived its delete")
	}
}

func grantCeiling(t *testing.T, h *grantHarness) {
	if err := h.repo.UpsertCeiling(h.ctx, persistence.AgentNamespaceBudget{Namespace: h.ns, CeilingUSD: 12, ApprovedByDevice: "dev_a", UpdatedAt: h.now}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.UpsertCeiling(h.ctx, persistence.AgentNamespaceBudget{Namespace: h.ns, CeilingUSD: 14.5, ApprovedByDevice: "dev_b", UpdatedAt: h.now}); err != nil {
		t.Fatal(err)
	}
	got, err := h.repo.GetCeiling(h.ctx, h.ns)
	if err != nil || got.CeilingUSD != 14.5 || got.ApprovedByDevice != "dev_b" {
		t.Fatalf("ceiling: %+v, %v", got, err)
	}
}

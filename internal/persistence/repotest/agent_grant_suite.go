package repotest

import (
	"context"
	"errors"
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
	t.Run("Account_columns", func(t *testing.T) { grantAccount(t, h) })
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

// grantAccount pins the account columns of agent_integration_approvals
// (GitHub #119 item 8 (2026-10-08): agent-administered design §22.2.2 and
// §22.2.14 item 1). An add writes the approved account; a re-grant of a live
// row never changes it; a re-add after a removal takes the new one and drops
// the old provider identity; SetIntegrationIdentity is the only writer of a
// provider identity and never touches the label. A row written without an
// account reads as the default account (operator decision F1/F4).
func grantAccount(t *testing.T, h *grantHarness) {
	get := func(name string) *persistence.AgentIntegrationApproval { return accountGet(t, h, name) }
	upsert := func(a persistence.AgentIntegrationApproval) { accountUpsert(t, h, a) }
	account := accountOf

	// A legacy-shaped write (no account) reads as the default account.
	legacy := persistence.AgentIntegrationApproval{Namespace: h.ns, ProjectID: h.project, Integration: "acc-legacy", Kind: "mcp",
		URL: "https://mail.example/mcp", ApprovedByDevice: "dev_a", ApprovedAt: h.now}
	upsert(legacy)
	if got := account(get("acc-legacy")); got != [3]string{"default", "", ""} {
		t.Fatalf("a row written without an account reads %q, want the default account", got)
	}

	// An add carries the approved account, round-tripped by Get and List.
	work := persistence.AgentIntegrationApproval{Namespace: h.ns, ProjectID: h.project, Integration: "acc-work", Kind: "mcp",
		URL: "https://mail.example/mcp", ApprovedByDevice: "dev_a", ApprovedAt: h.now,
		AccountLabel: "work", AccountIdentity: "alice@corp.example", IdentitySource: "agent"}
	upsert(work)
	if got := account(get("acc-work")); got != [3]string{"work", "alice@corp.example", "agent"} {
		t.Fatalf("round trip: %q", got)
	}
	list, err := h.repo.ListIntegrations(h.ctx, h.ns)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range list {
		if a.Integration == "acc-work" {
			found = a.AccountLabel == "work" && a.AccountIdentity == "alice@corp.example" && a.IdentitySource == "agent"
		}
	}
	if !found {
		t.Fatalf("ListIntegrations does not carry the account: %+v", list)
	}

	// SetIntegrationIdentity records a provider identity and never the label.
	if err := h.repo.SetIntegrationIdentity(h.ctx, h.project, "acc-work", "alice@corp.example"); err != nil {
		t.Fatal(err)
	}
	if got := account(get("acc-work")); got != [3]string{"work", "alice@corp.example", "provider"} {
		t.Fatalf("after a provider identity: %q", got)
	}

	// A re-grant of the live row (approve_server_tools, a narrowing) keeps
	// the account, whatever the grant carries.
	regrant := work
	regrant.ReadTools, regrant.AccountLabel, regrant.AccountIdentity, regrant.IdentitySource = []string{"read"}, "personal", "bob@home.example", "agent"
	upsert(regrant)
	got := get("acc-work")
	if account(got) != [3]string{"work", "alice@corp.example", "provider"} || !reflect.DeepEqual(got.ReadTools, []string{"read"}) {
		t.Fatalf("a re-grant of a live row changed its account or lost its tools: %+v", got)
	}

	// Clearing: an empty identity clears both identity columns.
	if err := h.repo.SetIntegrationIdentity(h.ctx, h.project, "acc-work", ""); err != nil {
		t.Fatal(err)
	}
	if got := account(get("acc-work")); got != [3]string{"work", "", ""} {
		t.Fatalf("after clearing: %q", got)
	}
	if err := h.repo.SetIntegrationIdentity(h.ctx, h.project, "acc-work", "alice@corp.example"); err != nil {
		t.Fatal(err)
	}
	grantAccountReAdd(t, h, work)
}

func accountGet(t *testing.T, h *grantHarness, name string) *persistence.AgentIntegrationApproval {
	t.Helper()
	got, err := h.repo.GetIntegration(h.ctx, h.project, name)
	if err != nil {
		t.Fatalf("GetIntegration %s: %v", name, err)
	}
	return got
}

func accountUpsert(t *testing.T, h *grantHarness, a persistence.AgentIntegrationApproval) {
	t.Helper()
	if err := h.repo.UpsertIntegration(h.ctx, a); err != nil {
		t.Fatal(err)
	}
}

func accountOf(got *persistence.AgentIntegrationApproval) [3]string {
	return [3]string{got.AccountLabel, got.AccountIdentity, got.IdentitySource}
}

// grantAccountReAdd continues grantAccount on its live row "acc-work",
// which holds account work with provider identity alice@corp.example.
func grantAccountReAdd(t *testing.T, h *grantHarness, work persistence.AgentIntegrationApproval) {
	get := func(name string) *persistence.AgentIntegrationApproval { return accountGet(t, h, name) }
	account := accountOf
	// A re-add after a removal takes the new account and drops the old
	// provider identity.
	if err := h.repo.MarkIntegrationRemoved(h.ctx, h.project, "acc-work", h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := account(get("acc-work")); got != [3]string{"work", "alice@corp.example", "provider"} {
		t.Fatalf("a removal must not touch the account: %q", got)
	}
	if err := h.repo.SetIntegrationIdentity(h.ctx, h.project, "acc-work", "x@y.example"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("SetIntegrationIdentity on a removed row: %v, want ErrNotFound", err)
	}
	readd := work
	readd.AccountLabel, readd.AccountIdentity, readd.IdentitySource = "personal", "", ""
	accountUpsert(t, h, readd)
	got := get("acc-work")
	if got.RemovedAt != nil || account(got) != [3]string{"personal", "", ""} {
		t.Fatalf("a re-add did not take the new account: %+v", got)
	}

	// No live row: ErrNotFound, and nothing is created.
	if err := h.repo.SetIntegrationIdentity(h.ctx, h.project, "acc-absent", "x@y.example"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("SetIntegrationIdentity on no row: %v, want ErrNotFound", err)
	}
	if _, err := h.repo.GetIntegration(h.ctx, h.project, "acc-absent"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("SetIntegrationIdentity created a row: %v", err)
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

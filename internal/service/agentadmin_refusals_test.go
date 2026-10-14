package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #172 (2026-10-13): refused agent-admin calls were invisible on the
// assistants page. A recorded refusal shows in the list's 7-day count and
// on the namespace's page by rule and as a recent row (design §23.2).
func TestAgentsView_ShowsRecordedRefusals(t *testing.T) {
	f := newAgentAdminFixture(t)
	if f.c.repos.AgentAdminRefusals == nil {
		t.Fatal("the fixture's repositories have no refusal ledger")
	}
	ctx := context.Background()
	if _, err := f.svc.EnsureHome(ctx, "hermes", "hermes"); err != nil {
		t.Fatal(err)
	}
	l := f.c.agentRefusalLedger()
	if l == nil || l != f.c.agentRefusalLedger() {
		t.Fatal("the ledger is not built once and shared")
	}
	l.Record(ctx, f.key, agentadmin.VerbAddAPI, agentadmin.RuleURLNotHTTPS)
	l.Record(ctx, f.key, agentadmin.VerbSetBudget, agentadmin.RuleOutOfNamespace)
	l.Record(ctx, f.key, agentadmin.VerbSetBudget, agentadmin.RuleOutOfNamespace)

	view := f.c.agentsView()
	rows, err := view.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Namespace == "hermes" {
			found = true
			if r.Refused7d != 3 {
				t.Fatalf("list row Refused7d = %d, want 3", r.Refused7d)
			}
		}
	}
	if !found {
		t.Fatalf("hermes not listed: %+v", rows)
	}
	page, err := view.DescribeAgent(ctx, "hermes")
	if err != nil || page == nil {
		t.Fatalf("describe: %v", err)
	}
	got := page.Refusals
	if got.Total != 3 || len(got.ByRule) != 2 || got.ByRule[0].Rule != agentadmin.RuleOutOfNamespace || got.ByRule[0].Count != 2 ||
		len(got.Recent) != 3 || got.Recent[0].KeyID != f.key.ID || got.Unwritten != 0 {
		t.Fatalf("refusals view = %+v", got)
	}
	// The doctor burst check's source counts the same rows.
	counts, err := f.c.agentRefusalCounts(ctx, time.Now().Add(-time.Hour))
	if err != nil || counts["hermes"] != 3 {
		t.Fatalf("doctor source = %v, %v", counts, err)
	}
}

// failingRefusalReads fails every read of the refusal ledger.
type failingRefusalReads struct {
	persistence.AgentAdminRefusalRepository
}

func (failingRefusalReads) ListRecent(context.Context, string, int) ([]persistence.AgentAdminRefusal, error) {
	return nil, errors.New("injected: refusal read failed")
}
func (failingRefusalReads) CountByRule(context.Context, string, time.Time) (map[string]int, error) {
	return nil, errors.New("injected: refusal read failed")
}
func (failingRefusalReads) CountByNamespace(context.Context, time.Time) (map[string]int, error) {
	return nil, errors.New("injected: refusal read failed")
}

// #334 audit 2026-10-14: a failed refusal-count read failed the whole
// /ui/admin/agents list and the assistant's page, hiding keys, projects
// and pending approvals behind a secondary count. The page now degrades
// the way the inbox does for its approvals load (#167): the count is left
// empty and marked unavailable, and the rest renders.
func TestAgentsView_RefusalReadFailureDegrades(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	if _, err := f.svc.EnsureHome(ctx, "hermes", "hermes"); err != nil {
		t.Fatal(err)
	}
	f.c.repos.AgentAdminRefusals = failingRefusalReads{f.c.repos.AgentAdminRefusals}
	view := f.c.agentsView()
	rows, err := view.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents failed on a refusal read: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Namespace == "hermes" {
			found = true
			if !r.RefusedUnavailable || r.Refused7d != 0 {
				t.Errorf("list row = %+v, want the refused count marked unavailable", r)
			}
		}
	}
	if !found {
		t.Fatalf("hermes not listed: %+v", rows)
	}
	page, err := view.DescribeAgent(ctx, "hermes")
	if err != nil || page == nil {
		t.Fatalf("DescribeAgent failed on a refusal read: %v", err)
	}
	if !page.Refusals.Unavailable || !page.Row.RefusedUnavailable || page.Refusals.Total != 0 {
		t.Errorf("page refusals = %+v, row = %+v; want unavailable", page.Refusals, page.Row)
	}
	if len(page.Projects) == 0 {
		t.Errorf("the rest of the page did not render: %+v", page)
	}
}

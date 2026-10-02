package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// Plan P6.5: /ui/admin/agents lists each namespace with its key's client
// kind, prefix and last use, and renders a namespace's setup from the same
// source as list_my_setup and describe_installation; a credential shows its
// name and status, never a value. An unknown namespace is nil. Control:
// agentsView.
func TestAgentsView(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	if _, err := f.svc.EnsureHome(ctx, "hermes", "hermes"); err != nil {
		t.Fatal(err)
	}
	used := time.Now().Add(-time.Hour)
	if err := f.c.repos.APIKeys.Create(ctx, &persistence.APIKey{ID: "akey-h", ProjectID: "hermes--home", Name: "hermes",
		KeyHash: "h", KeyPrefix: "sk-vornik-hermes--home.ab", ClientKind: "hermes", CreatedAt: time.Now(), LastUsedAt: &used,
		AgentAdmin: true, AgentNamespace: "hermes"}); err != nil {
		t.Fatal(err)
	}
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	f.approve(f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "finance", Name: "fio", BaseURL: "https://api.fio.example/v1",
		Auth: agentadmin.APIAuthInput{Credential: "FIO"}, Methods: []string{"GET"}}))

	v := agentsView{s: f.svc}
	rows, err := v.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Namespace != "hermes" || rows[0].ClientKind != "hermes" || rows[0].Class != agentadmin.HarnessMCPOnly ||
		rows[0].KeyStatus != "active" || rows[0].KeyPrefix != "sk-vornik-hermes--home.ab" || rows[0].LastUsed == "" || rows[0].Projects != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	page, err := v.DescribeAgent(ctx, "hermes")
	if err != nil || page == nil {
		t.Fatalf("describe: %v %v", page, err)
	}
	found := false
	for _, p := range page.Projects {
		if p.ID != "hermes--finance" {
			continue
		}
		found = true
		if p.Purpose != "Money" || len(p.Integrations) != 1 || p.Integrations[0].Name != "fio" || len(p.Credentials) != 1 ||
			p.Credentials[0].Name != "FIO" || p.Credentials[0].Status != "missing" {
			t.Fatalf("finance: %+v", p)
		}
	}
	if !found || len(page.Claims) == 0 || page.CeilingUSD == 0 {
		t.Fatalf("page: %+v", page)
	}
	if p, err := v.DescribeAgent(ctx, "nobody"); err != nil || p != nil {
		t.Fatalf("unknown namespace: %+v %v", p, err)
	}
	if p, err := v.DescribeAgent(ctx, "../x"); err != nil || p != nil {
		t.Fatalf("invalid namespace: %+v %v", p, err)
	}
	// After disconnect the namespace is still listed, as disconnected.
	if err := f.c.repos.APIKeys.Revoke(ctx, "akey-h"); err != nil {
		t.Fatal(err)
	}
	rows, _ = v.ListAgents(ctx)
	if len(rows) != 1 || rows[0].KeyStatus != "none" || rows[0].KeyPrefix != "" {
		t.Fatalf("after revoke: %+v", rows)
	}
}

// Plan P7.4: the assistants page shows a scheduled workflow's schedule in
// words and its next run. Control: agentProjectView's workflow detail.
func TestAgentProjectView_Schedule(t *testing.T) {
	next := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	v := agentProjectView(SetupProject{ID: "hermes--fin", Workflows: []SetupWorkflow{{ID: "hermes--fin--sum", Approved: true,
		Schedule: &SetupSchedule{Sentence: "at 08:00 on day 1 of every month (Europe/Prague)", NextRun: &next}}}})
	d := v.Workflows[0].Detail
	if !strings.Contains(d, "at 08:00 on day 1 of every month (Europe/Prague)") || !strings.Contains(d, "2026-11-01 07:00 UTC") {
		t.Fatalf("detail: %q", d)
	}
}

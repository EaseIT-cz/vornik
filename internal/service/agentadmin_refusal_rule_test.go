package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

// GitHub #172 (2026-10-13): a refused agent-admin verb left no row of its
// own. The service names the rule at the line that decides each of its five
// refusals, and passes the renderer's rule through (design §23.2).

func mustRule(t *testing.T, res agentadmin.Result, want string) {
	t.Helper()
	if res.Effect != agentadmin.EffectRefused || res.Rule != want {
		t.Fatalf("effect %q rule %q (reason %q), want refused with %q", res.Effect, res.Rule, res.Reason, want)
	}
}

func TestAgentAdminRule_VerbNotCallable(t *testing.T) {
	f := newAgentAdminFixture(t)
	mustRule(t, f.do(agentadmin.VerbApproveServerTools, map[string]any{}), agentadmin.RuleVerbNotCallable)
}

func TestAgentAdminRule_RendererRulePassesThrough(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	mustRule(t, f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "again"}), agentadmin.RuleAlreadyExists)
	// Another namespace's project: the refusal text says "no project", the
	// rule says what happened.
	mustRule(t, f.do(agentadmin.VerbSetBudget, agentadmin.SetBudgetInput{Project: "codex--finance", MonthlyUSD: 1}), agentadmin.RuleOutOfNamespace)
}

func TestAgentAdminRule_TransportMismatch(t *testing.T) {
	f := newAgentAdminFixture(t)
	showProbeOutcomes(t)
	scraper := newSSEOnlyMCP(t, "fetch_page")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "venues", Purpose: "Venue research"})
	mustRule(t, f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "venues", Name: "scraper", URL: scraper.URL}), agentadmin.RuleTransportMismatch)
}

func TestAgentAdminRule_SpendingBlocked(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.forceOver(8)
	mustRule(t, f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "xa", Purpose: "More"}), agentadmin.RuleSpendingBlocked)
}

func TestAgentAdminRule_ApplyRefused(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}})
	f.svc.engine.HasActiveTasks = func(context.Context, string) (bool, error) { return true, nil }
	input, _ := json.Marshal(agentadmin.RemoveInput{Kind: "integration", Project: "comms", ID: "mail"})
	res, err := f.svc.doVerb(context.Background(), f.key, agentadmin.VerbRemove, input)
	if err != nil {
		t.Fatal(err)
	}
	mustRule(t, res, agentadmin.RuleApplyRefused)
}

func TestAgentAdminRule_TooManyFiles(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.setCeiling(1000)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "big", Purpose: "Many workflows"})
	for i := 0; i < 11; i++ {
		res := f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "big", Slug: fmt.Sprintf("w%d", i),
			Steps:  []agentadmin.StepInput{{Name: "s", Role: "worker", Instructions: "x"}},
			Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"total":{"type":"number"}}}`)})
		if res.Effect == agentadmin.EffectAwaiting {
			f.approve(res)
		} else if res.Effect != agentadmin.EffectApplied {
			t.Fatalf("workflow %d: %+v", i, res)
		}
	}
	res := f.do(agentadmin.VerbRemove, agentadmin.RemoveInput{Kind: "project", ID: "big"})
	if !strings.Contains(res.Reason, "more than 12 files") {
		t.Fatalf("not the 12-file refusal: %+v", res)
	}
	mustRule(t, res, agentadmin.RuleTooManyFiles)
}

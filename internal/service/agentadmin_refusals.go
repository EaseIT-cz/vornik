package service

import (
	"context"
	"sort"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/ui"
)

// The refusal ledger's service side (agent-administered design §23.2,
// GitHub #172): one ledger shared by the companion endpoint (which writes)
// and the assistants page (which reads its in-process "not written" count).

// refusalPageWindow is the window of the assistants page's counts.
const refusalPageWindow = 7 * 24 * time.Hour

// refusalPageRecent is how many recent rows a namespace's page shows.
const refusalPageRecent = 20

// agentRefusalLedger returns the shared ledger, or nil without a repository.
func (c *Container) agentRefusalLedger() *api.AgentRefusalLedger {
	if c.repos == nil || c.repos.AgentAdminRefusals == nil {
		return nil
	}
	c.agentRefusalsOnce.Do(func() {
		c.agentRefusals = api.NewAgentRefusalLedger(c.repos.AgentAdminRefusals, c.Logger)
	})
	return c.agentRefusals
}

// agentRefusalCounts is the agent_refusal_burst doctor check's source.
func (c *Container) agentRefusalCounts(ctx context.Context, since time.Time) (map[string]int, error) {
	return c.repos.AgentAdminRefusals.CountByNamespace(ctx, since)
}

// refusalsView is a namespace's refusal section of the assistants page.
func (c *Container) refusalsView(ctx context.Context, ns string) (ui.AgentRefusals, error) {
	var out ui.AgentRefusals
	if c.repos == nil || c.repos.AgentAdminRefusals == nil {
		return out, nil
	}
	repo := c.repos.AgentAdminRefusals
	counts, err := repo.CountByRule(ctx, ns, time.Now().Add(-refusalPageWindow))
	if err != nil {
		return out, err
	}
	// The closed vocabulary's order, then anything else (a rule written by
	// another version), so the total is always the sum of the rows shown.
	seen := map[string]bool{}
	for _, r := range agentadmin.RefusalRules {
		seen[r] = true
		if n := counts[r]; n > 0 {
			out.ByRule = append(out.ByRule, ui.AgentRuleCount{Rule: r, Count: n})
		}
	}
	var extra []string
	for r, n := range counts {
		if !seen[r] && n > 0 {
			extra = append(extra, r)
		}
	}
	sort.Strings(extra)
	for _, r := range extra {
		out.ByRule = append(out.ByRule, ui.AgentRuleCount{Rule: r, Count: counts[r]})
	}
	for _, rc := range out.ByRule {
		out.Total += rc.Count
	}
	rows, err := repo.ListRecent(ctx, ns, refusalPageRecent)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out.Recent = append(out.Recent, ui.AgentRefusalRow{When: r.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"), Verb: r.Verb, Rule: r.Rule, KeyID: r.KeyID})
	}
	out.Unwritten = c.agentRefusalLedger().Unwritten(ns)
	return out, nil
}

// refusedCounts7d is every namespace's 7-day refusal count, for the list.
func (c *Container) refusedCounts7d(ctx context.Context) (map[string]int, error) {
	if c.repos == nil || c.repos.AgentAdminRefusals == nil {
		return nil, nil
	}
	return c.repos.AgentAdminRefusals.CountByNamespace(ctx, time.Now().Add(-refusalPageWindow))
}

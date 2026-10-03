package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
)

// agentsView serves /ui/admin/agents (agent-administered Vornik design
// §10.3, plan P6.5) from the same source as list_my_setup and
// describe_installation, so the operator sees what the agent sees.
type agentsView struct{ s *agentAdminService }

// agentsView builds the adapter, or nil when the service cannot be wired.
// The service itself is resolved per request, so the pages appear once the
// agent templates are installed (see agentAdmin).
func (c *Container) agentsView() ui.AgentsSource {
	if !c.agentAdminWireable() || c.repos.APIKeys == nil {
		return nil
	}
	return lazyAgentsView{c: c}
}

// lazyAgentsView resolves the agent admin service on each request.
type lazyAgentsView struct{ c *Container }

func (v lazyAgentsView) ListAgents(ctx context.Context) ([]ui.AgentRow, error) {
	s := v.c.agentAdmin()
	if s == nil {
		return nil, agentadmin.ErrUnavailable
	}
	return agentsView{s: s}.ListAgents(ctx)
}

func (v lazyAgentsView) DescribeAgent(ctx context.Context, ns string) (*ui.AgentPage, error) {
	s := v.c.agentAdmin()
	if s == nil {
		return nil, agentadmin.ErrUnavailable
	}
	return agentsView{s: s}.DescribeAgent(ctx, ns)
}

// WithdrawModelDestination implements ui.ModelWithdrawer.
func (v lazyAgentsView) WithdrawModelDestination(ctx context.Context, ns, destination string) (bool, error) {
	s := v.c.agentAdmin()
	if s == nil {
		return false, agentadmin.ErrUnavailable
	}
	return agentsView{s: s}.WithdrawModelDestination(ctx, ns, destination)
}

// WithdrawModelDestination sets removed_at on a namespace's model
// destination approval: the only writer of removed_at (design §18.6 item 2,
// round 3 F3; a source test pins it). The row and its history stay; the
// run-time check treats it as absent, and approving it again is a new
// widening.
func (v agentsView) WithdrawModelDestination(ctx context.Context, ns, destination string) (bool, error) {
	if !agentns.Valid(ns) {
		return false, fmt.Errorf("invalid namespace %q", ns)
	}
	return v.s.grants.MarkModelDestinationRemoved(ctx, ns, destination, time.Now().UTC())
}

// namespaces lists every agent namespace with at least one project.
func (v agentsView) namespaces() []string {
	seen := map[string]bool{}
	for _, p := range v.s.c.Registry.ListProjects() {
		if p == nil {
			continue
		}
		if ns, ok := agentns.FromID(p.ID); ok {
			seen[ns] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// liveKey is the namespace's active agent admin key, nil when there is none
// (disconnected).
func (v agentsView) liveKey(ctx context.Context, ns string) (*persistence.APIKey, error) {
	keys, err := v.s.c.repos.APIKeys.ListByProject(ctx, agentns.ID(ns, "home"))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, k := range keys {
		if k != nil && k.AgentAdmin && k.AgentNamespace == ns && k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(now)) {
			return k, nil
		}
	}
	return nil, nil
}

func (v agentsView) row(ctx context.Context, ns string) (ui.AgentRow, SetupView, error) {
	key, err := v.liveKey(ctx, ns)
	if err != nil {
		return ui.AgentRow{}, SetupView{}, err
	}
	row := ui.AgentRow{Namespace: ns, KeyStatus: "none"}
	// The setup is read as the namespace; a disconnected one still has it.
	reader := &persistence.APIKey{AgentAdmin: true, AgentNamespace: ns, ProjectID: agentns.ID(ns, "home")}
	if key != nil {
		row.ClientKind, row.KeyStatus, row.KeyPrefix = key.ClientKind, "active", key.KeyPrefix
		if key.LastUsedAt != nil {
			row.LastUsed = key.LastUsedAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		reader.ClientKind = key.ClientKind
	}
	row.Class = agentadmin.HarnessClassOf(row.ClientKind)
	setup, err := v.s.ListSetup(ctx, reader)
	if err != nil {
		return row, setup, err
	}
	row.Projects, row.Pending = len(setup.Projects), len(setup.Pending)
	return row, setup, nil
}

// ListAgents implements ui.AgentsSource.
func (v agentsView) ListAgents(ctx context.Context) ([]ui.AgentRow, error) {
	var out []ui.AgentRow
	for _, ns := range v.namespaces() {
		row, _, err := v.row(ctx, ns)
		if err != nil {
			return out, fmt.Errorf("%s: %w", ns, err)
		}
		out = append(out, row)
	}
	return out, nil
}

// DescribeAgent implements ui.AgentsSource.
func (v agentsView) DescribeAgent(ctx context.Context, ns string) (*ui.AgentPage, error) {
	if !agentns.Valid(ns) || v.s.c.Registry.GetProject(agentns.ID(ns, "home")) == nil {
		return nil, nil
	}
	row, setup, err := v.row(ctx, ns)
	if err != nil {
		return nil, err
	}
	page := &ui.AgentPage{Row: row, Claims: agentadmin.HarnessClaims(row.Class), BudgetUSD: setup.BudgetUSD, CeilingUSD: setup.CeilingUSD,
		Pending: requestsView(setup.Pending), Failed: requestsView(setup.Failed), Expired: requestsView(setup.Expired)}
	for _, p := range setup.Projects {
		page.Projects = append(page.Projects, agentProjectView(p))
	}
	dests, err := v.s.grants.ListModelDestinations(ctx, ns)
	if err != nil {
		return nil, err
	}
	for _, d := range dests {
		status := "approved"
		if d.RemovedAt != nil {
			status = "withdrawn"
		}
		page.Models = append(page.Models, ui.AgentItem{Name: d.Destination, Status: status})
	}
	return page, nil
}

func agentProjectView(p SetupProject) ui.AgentProject {
	out := ui.AgentProject{ID: p.ID, Purpose: p.Purpose, BudgetUSD: p.BudgetUSD}
	for _, w := range p.Workflows {
		status := "waiting for approval"
		if w.Approved {
			status = "approved"
		}
		item := ui.AgentItem{Name: w.ID, Status: status}
		if sc := w.Schedule; sc != nil {
			item.Detail = "runs automatically " + sc.Sentence
			if sc.NextRun != nil {
				item.Detail += "; next " + sc.NextRun.UTC().Format("2006-01-02 15:04 UTC")
			}
		}
		out.Workflows = append(out.Workflows, item)
	}
	for _, s := range p.Servers {
		out.Integrations = append(out.Integrations, ui.AgentItem{Name: s.Name, Status: s.Status, Detail: s.URL})
	}
	for _, a := range p.APIs {
		out.Integrations = append(out.Integrations, ui.AgentItem{Name: a.Name, Status: a.Status, Detail: a.BaseURL})
	}
	for _, c := range p.Credentials {
		out.Credentials = append(out.Credentials, ui.AgentItem{Name: c.Name, Status: c.Status})
	}
	return out
}

func requestsView(rs []SetupRequest) []ui.AgentRequest {
	out := make([]ui.AgentRequest, 0, len(rs))
	for _, r := range rs {
		out = append(out, ui.AgentRequest{Sentence: r.Sentence, When: r.Created.UTC().Format("2006-01-02 15:04 UTC")})
	}
	return out
}

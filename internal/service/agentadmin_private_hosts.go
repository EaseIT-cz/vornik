package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/persistence"
)

// agentPrivateHostsStatus is the source of the doctor finding
// agent_private_hosts_unused (agent-administered design part 06 §6.12 item 8,
// GitHub #403): every agent_admin.private_api_hosts entry against the live
// approved connections of every agent namespace with a loaded project.
func (c *Container) agentPrivateHostsStatus(ctx context.Context) (api.AgentPrivateHostsStatus, error) {
	if c.Registry == nil || c.repos == nil || c.repos.AgentGrants == nil {
		return api.AgentPrivateHostsStatus{}, errors.New("the registry or the approval tables are not available")
	}
	return unusedPrivateHosts(ctx, c.loadedAgentNamespaces(), c.repos.AgentGrants.ListIntegrations, c.agentPrivateAPIHosts())
}

// loadedAgentNamespaces is the sorted agent namespaces of the loaded
// projects.
func (c *Container) loadedAgentNamespaces() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range c.Registry.ListProjects() {
		if ns, ok := agentns.FromID(p.ID); ok && !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	sort.Strings(out)
	return out
}

// unusedPrivateHosts marks an entry used when it is the grant the dial would
// take for a live (not removed) connection's URL in that connection's
// project: the first entry, in file order, that serves the project and
// matches (dialguard.PrivateHostGrant). It resolves nothing.
func unusedPrivateHosts(ctx context.Context, namespaces []string,
	list func(context.Context, string) ([]persistence.AgentIntegrationApproval, error),
	entries []dialguard.PrivateHost,
) (api.AgentPrivateHostsStatus, error) {
	st := api.AgentPrivateHostsStatus{Entries: len(entries)}
	used := make([]bool, len(entries))
	for _, ns := range namespaces {
		rows, err := list(ctx, ns)
		if err != nil {
			return api.AgentPrivateHostsStatus{}, err
		}
		for _, r := range rows {
			if r.RemovedAt != nil {
				continue
			}
			st.Connections++
			for i, e := range entries {
				if e.Serves(r.ProjectID) && dialguard.PrivateHostMatches(r.URL, e) {
					used[i] = true
					break
				}
			}
		}
	}
	for i, e := range entries {
		if !used[i] {
			st.Unused = append(st.Unused, describeEntry(e))
		}
	}
	return st, nil
}

// describeEntry is an entry with its projects, for the doctor.
func describeEntry(e dialguard.PrivateHost) string {
	if len(e.Projects) == 0 {
		return e.String()
	}
	return e.String() + " (projects " + strings.Join(e.Projects, ", ") + ")"
}

// intranetHostsFor is describe_installation's intranet_hosts (§6.12 item 9):
// each entry serving one of namespace ns's projects, as host[:port] with the
// projects it serves. Another namespace's project is never named, and pins
// are not shown.
func intranetHostsFor(ns string, entries []dialguard.PrivateHost) []string {
	out := []string{}
	for _, e := range entries {
		if len(e.Projects) == 0 {
			out = append(out, e.HostPort()+" (every project)")
			continue
		}
		var mine []string
		for _, p := range e.Projects {
			if got, ok := agentns.FromID(p); ok && got == ns {
				mine = append(mine, p)
			}
		}
		if len(mine) > 0 {
			out = append(out, e.HostPort()+" (projects "+strings.Join(mine, ", ")+")")
		}
	}
	return out
}

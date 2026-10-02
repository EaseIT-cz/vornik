package agentadmin

import (
	"strings"

	"vornik.io/vornik/internal/registry"
)

// ProjectStateFrom converts a loaded agent project and its swarm into the
// renderer's state. The service and the tests build State through it, so
// both see a project the way the renderer will re-render it.
func ProjectStateFrom(p *registry.Project, sw *registry.Swarm) *ProjectState {
	ps := &ProjectState{
		ID: p.ID, DisplayName: p.DisplayName, Purpose: strings.TrimSpace(p.Description),
		MonthlyUSD: p.Budget.MonthlyHardUSD, DefaultWorkflowID: p.DefaultWorkflowID,
		Secrets: append([]string(nil), p.Permissions.Secrets...), Loaded: p, LoadedSwarm: sw,
	}
	ps.Swarm.ID = p.SwarmID
	for _, a := range p.APIs {
		ps.APIs = append(ps.APIs, APIState{Name: a.Name, BaseURL: a.BaseURL, Header: a.Auth.Header,
			AuthRef: strings.TrimSpace(a.Auth.ValueFrom), Prefix: a.Auth.Prefix,
			Methods: append([]string(nil), a.Methods...), Writes: a.Writes})
	}
	for _, s := range p.MCP.Servers {
		ps.Servers = append(ps.Servers, ServerState{
			Name: s.Name, URL: s.URL, AuthRef: strings.TrimSpace(s.Auth.ValueFrom),
			OAuth: s.Auth.Mode == "oauth", Scopes: append([]string(nil), s.Auth.Scopes...),
			Write: s.BrokerWrite, Tools: append([]string(nil), s.AllowedTools...),
		})
	}
	if sw != nil {
		for _, r := range sw.Roles {
			ps.Swarm.Roles = append(ps.Swarm.Roles, RoleSpec{
				Name: r.Name, Description: r.Description,
				Tools: append([]string(nil), r.Permissions.AllowedTools...),
			})
		}
	}
	return ps
}

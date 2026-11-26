package agentadmin

import (
	"strconv"
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
		// Loaded back so a verb that re-renders the project keeps it (§6.9).
		MaxConcurrentTasks: p.MaxConcurrentTasks,
		SharedMemory:       p.Memory.SharedWithNamespace, // part 24 §24.6
		Secrets:            append([]string(nil), p.Permissions.Secrets...), Loaded: p, LoadedSwarm: sw,
	}
	ps.Swarm.ID = p.SwarmID
	for _, a := range p.APIs {
		ps.APIs = append(ps.APIs, APIState{Name: a.Name, BaseURL: a.BaseURL, Header: a.Auth.Header,
			QueryParam: a.Auth.QueryParam, AuthRef: strings.TrimSpace(a.Auth.ValueFrom), Prefix: a.Auth.Prefix,
			Methods: append([]string(nil), a.Methods...), Writes: a.Writes,
			// Loaded back so a verb that re-renders the project keeps it
			// (design §22.2.1).
			Account: a.Account})
	}
	for _, s := range p.MCP.Servers {
		ps.Servers = append(ps.Servers, ServerState{
			Name: s.Name, URL: s.URL, AuthRef: strings.TrimSpace(s.Auth.ValueFrom),
			OAuth: s.Auth.Mode == "oauth", Scopes: append([]string(nil), s.Auth.Scopes...),
			Write: s.BrokerWrite, Tools: append([]string(nil), s.AllowedTools...),
			// Loaded back so a verb that re-renders the project keeps it
			// (design §6.1).
			Transport: s.Transport,
			Account:   s.Account,
		})
	}
	if sw != nil {
		for _, r := range sw.Roles {
			ps.Swarm.Roles = append(ps.Swarm.Roles, RoleSpec{
				// The instructions too (§18.11): without them, every verb that
				// re-renders the swarm wrote each role back with no text.
				Name: r.Name, Description: r.Description, Instructions: r.SystemPrompt,
				Tools: append([]string(nil), r.Permissions.AllowedTools...),
				// The model too (design §18.6 item 2, Change 8): a re-render
				// writes the role back on the model it runs on.
				Model: r.Model,
				// The limits too (§6.9): a re-render keeps what was approved.
				MaxTokens: r.MaxTokens, MaxToolIterations: roleToolIterations(r),
			})
		}
	}
	return ps
}

// roleToolIterations is the role's VORNIK_MAX_TOOL_ITERATIONS, 0 when unset
// or not a positive number (the executor's roleToolBudgetBase reads the
// same env var).
func roleToolIterations(r registry.SwarmRole) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.Runtime.EnvVars[toolIterationsEnv]))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// toolIterationsEnv is the agent's tool-iteration limit, read by its
// entrypoint (default 30).
const toolIterationsEnv = "VORNIK_MAX_TOOL_ITERATIONS"

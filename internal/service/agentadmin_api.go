package service

import (
	"net/http"

	"vornik.io/vornik/internal/agentapi"
	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/integrations/dialguard"
)

// Agent projects' REST clients (agent-administered Vornik plan P4.5).

// agentAPIReader is a role's client: GET and HEAD against the approved READ
// set, credential injected at call time, dialled through the SSRF guard.
func (c *Container) agentAPIReader(projectID string) apigateway.Client {
	cl := c.agentAPIClient(projectID, false, map[string]bool{http.MethodGet: true, http.MethodHead: true})
	if cl == nil {
		return nil // a nil interface, not a typed nil: the route reports "not configured"
	}
	return cl
}

// agentAPIClient builds a client for one project and route.
func (c *Container) agentAPIClient(projectID string, write bool, methods map[string]bool) *agentapi.Client {
	if c.Registry == nil || c.repos == nil || c.repos.AgentGrants == nil {
		return nil
	}
	return &agentapi.Client{
		ProjectID: projectID, Project: c.Registry.GetProject, Grants: c.repos.AgentGrants,
		Secrets: c.secretSource(), AllowedMethods: methods, Write: write,
		HTTP: func(base string) *http.Client {
			return c.agentServerClient(projectID, base, mcpOAuthHTTPTimeout)
		}, // #560 M1c D5; #734; #740
		Logger: c.Logger.With().Str("component", "agent-api").Str("project", projectID).Logger(),
	}
}

// agentPrivateAPIHosts is agent_admin.private_api_hosts, parsed, for every
// project (design part 06, §6.7, GitHub #734; §6.12, GitHub #403).
func (c *Container) agentPrivateAPIHosts() []dialguard.PrivateHost {
	if c.Config == nil {
		return nil
	}
	return c.Config.AgentAdmin.PrivateHosts()
}

// agentPrivateHostsFor is the entries that serve one agent project: the one
// place the service scopes the list (§6.12 answer 5). Every dial, listing
// mask and doctor test of an agent connection reads it.
func (c *Container) agentPrivateHostsFor(projectID string) []dialguard.PrivateHost {
	return dialguard.PrivateHostsFor(projectID, c.agentPrivateAPIHosts())
}

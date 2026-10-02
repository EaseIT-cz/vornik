package service

import (
	"net/http"

	"vornik.io/vornik/internal/agentapi"
	"vornik.io/vornik/internal/apigateway"
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
		HTTP:   func(base string) *http.Client { return agentDialGuard(base).HTTPClient(mcpOAuthHTTPTimeout) },
		Logger: c.Logger.With().Str("component", "agent-api").Str("project", projectID).Logger(),
	}
}

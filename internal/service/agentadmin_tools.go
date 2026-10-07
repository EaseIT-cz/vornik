package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
)

// Plan P4.3 (P3 amendment 5): a read-pending agent server's tools are listed
// once it can be reached with its credential, and the list is approved on
// the device through the daemon-internal verb approve_server_tools.

// listToolsTimeout bounds one listing.
const listToolsTimeout = 15 * time.Second

// AgentSystemActor files the changes the daemon makes on an agent's behalf
// (a tools approval); a device still approves each one.
const AgentSystemActor = "system:agent-admin"

// agentDialGuard is the SSRF guard for an agent server. DialGuard skips its
// private-address check for an allow-listed host, so only a URL that is
// itself loopback (a local server the person approved by its URL) is
// allow-listed; every other host must resolve to a public address, at
// listing and at every call.
func agentDialGuard(rawURL string) integrations.DialGuard {
	u, err := url.Parse(rawURL)
	if err != nil {
		return integrations.DialGuard{}
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return integrations.DialGuard{AllowedHosts: []string{host}}
	}
	return integrations.DialGuard{}
}

// guardAgentServer dials an agent project's server through agentDialGuard.
// Operator projects keep today's client.
func guardAgentServer(cfg *mcp.ServerConfig, projectID string) {
	if _, agent := agentns.FromID(projectID); !agent || cfg.URL == "" {
		return
	}
	timeout := 30 * time.Second
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	cfg.HTTPClient = agentDialGuard(cfg.URL).HTTPClient(timeout)
}

// listMCPTools connects once, through the guard, and returns the advertised
// tool names, sorted. Errors name the server, never a header value.
func (c *Container) listMCPTools(ctx context.Context, cfg mcp.ServerConfig) ([]string, error) {
	cfg.HTTPClient = agentDialGuard(cfg.URL).HTTPClient(listToolsTimeout)
	cctx, cancel := context.WithTimeout(ctx, listToolsTimeout)
	defer cancel()
	cl, err := mcp.Connect(cctx, cfg, c.Logger)
	if err != nil {
		return nil, fmt.Errorf("list the tools of %s: %w", cfg.Name, err)
	}
	defer func() { _ = cl.Close() }()
	var names []string
	for _, t := range cl.Tools() {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names, nil
}

// errCredentialMissing means the server's credential is not stored yet.
var errCredentialMissing = errors.New("the server's credential is not stored yet")

// serverConfigFor builds the connection config of an agent server with its
// credential, resolved the way the MCP manager resolves it.
func (c *Container) serverConfigFor(projectID, name, rawURL string, auth mcpauth.Auth, secrets []string) (mcp.ServerConfig, error) {
	cfg := mcp.ServerConfig{Name: name, Transport: "streamable-http", URL: rawURL, ProjectID: projectID}
	if auth.EffectiveMode() == mcpauth.ModeStatic {
		ref, _ := mcpauth.ParseSecretRef(auth.ValueFrom)
		if _, ok := c.secretSource().Get(ref); !ok {
			return cfg, errCredentialMissing
		}
	}
	if !c.applyMCPAuth(&cfg, auth, mcpGrantsFor(projectID, secrets), projectID, "project "+projectID) {
		return cfg, errCredentialMissing
	}
	return cfg, nil
}

// listingConfig is the connection the pre-approval listing makes. The project
// is the slug or the full id, as the verb itself accepts, never ns--ns--x.
func listingConfig(ns string, in agentadmin.AddMCPServerInput) mcp.ServerConfig {
	return mcp.ServerConfig{Name: in.Name, Transport: "streamable-http", URL: in.URL, ProjectID: agentns.ID(ns, agentadmin.ProjectSlug(ns, in.Project))}
}

// advertiseUnauthenticated lists an add_mcp_server target BEFORE approval,
// only when it carries no credential: listing a credentialed server first
// would send a stored value to a URL no person has approved. The result
// fills the verb's state, so the add asks for its tools in one approval.
func (s *agentAdminService) advertiseUnauthenticated(ctx context.Context, ns string, input json.RawMessage) (string, []string) {
	var in agentadmin.AddMCPServerInput
	if json.Unmarshal(input, &in) != nil || in.URL == "" {
		return "", nil
	}
	if in.Auth.Mode != "" && in.Auth.Mode != "none" {
		return "", nil
	}
	cfg := listingConfig(ns, in)
	tools, err := s.c.listMCPTools(ctx, cfg)
	if err != nil {
		s.c.Logger.Info().Str("namespace", ns).Str("server", in.Name).Err(err).Msg("agent admin: could not list a new server's tools; it will be read-pending")
		return "", nil
	}
	return in.URL, tools
}

// afterCredentialStored files a tools approval for each read-pending server
// of ns that uses ns/name, now that it can be reached (plan P4.3 trigger).
func (s *agentAdminService) afterCredentialStored(ctx context.Context, ns, name string) {
	ref := "secret://" + ns + "/" + name
	for _, p := range s.c.Registry.ListProjects() {
		if got, ok := agentns.FromID(p.ID); !ok || got != ns {
			continue
		}
		for _, srv := range p.MCP.Servers {
			if srv.Auth.ValueFrom != ref {
				continue
			}
			s.fileServerTools(ctx, ns, p.ID, srv.Name, srv.URL, srv.Auth, p.Permissions.Secrets)
		}
	}
}

// fileServerTools lists one approved, read-pending server and files the
// approval of what it listed. Nothing is filed when the listing fails or
// lists nothing: the server stays read-pending, and the reason is logged.
func (s *agentAdminService) fileServerTools(ctx context.Context, ns, projectID, server, rawURL string, auth mcpauth.Auth, secrets []string) {
	log := s.c.Logger.With().Str("namespace", ns).Str("project", projectID).Str("server", server).Logger()
	a, err := s.grants.GetIntegration(ctx, projectID, server)
	if err != nil || a.RemovedAt != nil || !a.ReadPending {
		return
	}
	cfg, err := s.c.serverConfigFor(projectID, server, rawURL, auth, secrets)
	if err != nil {
		log.Info().Err(err).Msg("agent admin: the server cannot be listed yet")
		return
	}
	tools, err := s.c.listMCPTools(ctx, cfg)
	if err != nil || len(tools) == 0 {
		log.Warn().Err(err).Int("tools", len(tools)).Msg("agent admin: the connected server listed no usable tools; it stays read-pending")
		return
	}
	raw, _ := json.Marshal(agentadmin.ApproveServerToolsInput{Project: projectID, Server: server, Tools: tools})
	res, err := s.doInternal(ctx, ns, agentadmin.VerbApproveServerTools, raw)
	if err != nil {
		log.Error().Err(err).Msg("agent admin: filing the tools approval failed")
		return
	}
	if res.Effect == EffectRefused {
		log.Info().Str("reason", res.Reason).Msg("agent admin: the tools approval was not filed")
		if len(res.MissingTools) > 0 {
			// A recipe's tools-approval check (design §19.8 F6): the gap is
			// shown in list_my_setup, built from the typed field, never from
			// the refusal's text (review 20261003-6b46 F2).
			s.setToolGap(projectID, server, "the server does not offer "+strings.Join(res.MissingTools, ", ")+"; this recipe needs them")
		}
		return
	}
	s.setToolGap(projectID, server, "")
}

// mcpServerConfigFor is a credential-less listing of url for namespace ns.
func mcpServerConfigFor(ns, url string) mcp.ServerConfig {
	return mcp.ServerConfig{Name: "recipe", Transport: "streamable-http", URL: url, ProjectID: agentns.ID(ns, "home")}
}

// doInternal renders and files a daemon-internal verb for ns. A device
// approves it like any widening change; the ledger names the system actor.
func (s *agentAdminService) doInternal(ctx context.Context, ns, verb string, input json.RawMessage) (AgentAdminResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// loadState reads the whole namespace; the second argument only marks
	// which project is the home one (review 20261002-a5d8 F7).
	st, err := s.loadState(ctx, ns, agentns.ID(ns, "home"))
	if err != nil {
		return AgentAdminResult{}, err
	}
	ch, err := s.renderer.Render(st, verb, input)
	if err != nil {
		return AgentAdminResult{}, err
	}
	if ch.Class != agentadmin.Widening {
		return AgentAdminResult{Effect: EffectRefused, Reason: ch.Reason, MissingTools: ch.MissingTools}, nil
	}
	p, err := s.fileProposal(ctx, AgentSystemActor, "", ch)
	if err != nil {
		return AgentAdminResult{}, err
	}
	return s.requestApproval(ctx, p, ch)
}

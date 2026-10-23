package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"syscall"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
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
//
// The rule lives in integrations.LoopbackURLGuard since 2026-10-21 (GitHub
// #433: the trading panel's broker fetch uses the same one).
func agentDialGuard(rawURL string) integrations.DialGuard {
	return integrations.LoopbackURLGuard(rawURL, nil)
}

// loopbackAgentURL reports whether rawURL is one agentDialGuard allow-lists.
// An unparsable URL counts as loopback, the side that tells the agent less.
func loopbackAgentURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err != nil || integrations.IsLoopbackHost(u.Hostname())
}

// probeOutcomeVisible reports whether the agent may learn how a
// pre-approval listing of rawURL failed (design §6.1 item 4, amended
// 2026-10-12, audit finding G). A loopback URL is dialled before any person
// approved it, so its outcome classes would make add_mcp_server a port
// probe of the daemon host: every failure there reads the same. A variable
// so a test can treat its loopback server as a public host.
var probeOutcomeVisible = func(rawURL string) bool { return !loopbackAgentURL(rawURL) }

// uniformLoopbackFailure is the one failure class a loopback listing gets.
const uniformLoopbackFailure = "could not connect"

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

// toolListing is what one listing advertised: every tool name, and the
// subset Vornik's classifier calls read-only (design §6.4, GitHub #405).
type toolListing struct {
	names []string
	reads []string
}

// listMCPTools connects once, through the guard, and returns the advertised
// tool names, sorted, with the ones mcp.Tool.IsMutating calls read-only,
// judged on each tool's full descriptor (annotations and input schema).
// Errors name the server, never a header value.
func (c *Container) listMCPTools(ctx context.Context, cfg mcp.ServerConfig) (toolListing, error) {
	cfg.HTTPClient = agentDialGuard(cfg.URL).HTTPClient(listToolsTimeout)
	cctx, cancel := context.WithTimeout(ctx, listToolsTimeout)
	defer cancel()
	cl, err := mcp.Connect(cctx, cfg, c.Logger)
	if err != nil {
		return toolListing{}, fmt.Errorf("list the tools of %s: %w", cfg.Name, err)
	}
	defer func() { _ = cl.Close() }()
	var out toolListing
	for _, t := range cl.Tools() {
		out.names = append(out.names, t.Name)
		if !t.IsMutating() {
			out.reads = append(out.reads, t.Name)
		}
	}
	sort.Strings(out.names)
	sort.Strings(out.reads)
	return out, nil
}

// readsByName is the classifier's verdict where nothing was listed (the
// connector install approves recipe-declared tools): the name rule alone,
// the only part available without a descriptor (design §6.4 item 2).
func readsByName(tools []string) []string {
	var reads []string
	for _, n := range tools {
		if !(mcp.Tool{Name: n}).IsMutating() {
			reads = append(reads, n)
		}
	}
	return reads
}

// errCredentialMissing means the server's credential is not stored yet.
var errCredentialMissing = errors.New("the server's credential is not stored yet")

// errServerNotDialled means the wiring marked the server not to be dialled
// (an oauth server with no grant, auth design §14); the error carries the
// reason (review 5ed3 A6).
var errServerNotDialled = errors.New("the server is not dialled")

// agentTransport is the client transport of an agent server: its declared
// one, streamable-http when none (design §6.1).
func agentTransport(t string) string {
	if t == "" {
		return agentadmin.TransportStreamableHTTP
	}
	return t
}

// serverConfigFor builds the connection config of an agent server with its
// credential, resolved the way the MCP manager resolves it, over the
// server's declared transport (design §6.1).
func (c *Container) serverConfigFor(projectID, name, rawURL, transport string, auth mcpauth.Auth, secrets []string) (mcp.ServerConfig, error) {
	cfg := mcp.ServerConfig{Name: name, Transport: agentTransport(transport), URL: rawURL, ProjectID: projectID}
	if auth.EffectiveMode() == mcpauth.ModeStatic {
		ref, _ := mcpauth.ParseSecretRef(auth.ValueFrom)
		if _, ok := c.secretSource().Get(ref); !ok {
			return cfg, errCredentialMissing
		}
	}
	if !c.applyMCPAuth(&cfg, auth, mcpGrantsFor(projectID, secrets), projectID, "project "+projectID) {
		return cfg, errCredentialMissing
	}
	if cfg.NotDialledReason != "" {
		return cfg, fmt.Errorf("%w: %s", errServerNotDialled, cfg.NotDialledReason)
	}
	return cfg, nil
}

// listingConfig is the connection the pre-approval listing makes. The project
// is the slug or the full id, as the verb itself accepts, never ns--ns--x.
func listingConfig(ns string, in agentadmin.AddMCPServerInput) mcp.ServerConfig {
	return mcp.ServerConfig{Name: in.Name, Transport: agentTransport(in.Transport), URL: in.URL, ProjectID: agentns.ID(ns, agentadmin.ProjectSlug(ns, in.Project))}
}

// wrongTransportHint is the refusal (and tools-pending note) for a server
// that answered a streamable-HTTP initialize with 404 or 405 (design §6.1
// item 4; reference host 2026-10-08, hermes--prague-venue-research). The
// daemon never falls back to SSE by itself: the transport is part of what
// the person approves.
func wrongTransportHint(cfg mcp.ServerConfig, err error) string {
	var ce *mcp.CallError
	if cfg.Transport != agentadmin.TransportStreamableHTTP || !errors.As(err, &ce) ||
		(ce.Status != http.StatusNotFound && ce.Status != http.StatusMethodNotAllowed) {
		return ""
	}
	return fmt.Sprintf("the server at %s did not answer streamable HTTP (initialize returned %d); "+
		"if it is an SSE server, call add_mcp_server again with transport \"sse\"; otherwise check the URL", cfg.URL, ce.Status)
}

// listFailureClass names why a listing failed, for the approval sentence
// (design §6.1, review 03b6 finding 1). Never carries server bytes.
func listFailureClass(err error) string {
	var ce *mcp.CallError
	var ne net.Error
	switch {
	case errors.As(err, &ce) && ce.Status != 0:
		return fmt.Sprintf("HTTP %d", ce.Status)
	case errors.Is(err, integrations.ErrDialRefused):
		return "refused by the address guard"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timed out"
	default:
		return "could not connect"
	}
}

// unauthListing is the outcome of the pre-approval listing of an add.
type unauthListing struct {
	url    string
	tools  []string
	reads  []string // the classifier's read-only subset of tools (§6.4)
	failed string   // the failure's class, when the listing failed
	refuse string   // the refusal, when the server does not speak the transport
}

// advertiseUnauthenticated lists an add_mcp_server target BEFORE approval,
// only when it carries no credential: listing a credentialed server first
// would send a stored value to a URL no person has approved. The result
// fills the verb's state, so the add asks for its tools in one approval.
// A streamable-HTTP 404/405 is a refusal with the transport hint; any other
// failure is recorded by class, for the sentence (design §6.1).
func (s *agentAdminService) advertiseUnauthenticated(ctx context.Context, ns string, input json.RawMessage) unauthListing {
	var in agentadmin.AddMCPServerInput
	if json.Unmarshal(input, &in) != nil || in.URL == "" {
		return unauthListing{}
	}
	if in.Auth.Mode != "" && in.Auth.Mode != "none" {
		return unauthListing{}
	}
	if in.Transport != "" && in.Transport != agentadmin.TransportStreamableHTTP && in.Transport != agentadmin.TransportSSE {
		return unauthListing{} // the renderer refuses it, naming the allowed values
	}
	cfg := listingConfig(ns, in)
	listed, err := s.c.listMCPTools(ctx, cfg)
	if err != nil {
		s.c.Logger.Info().Str("namespace", ns).Str("server", in.Name).Err(err).Msg("agent admin: could not list a new server's tools; it will be read-pending")
		if !probeOutcomeVisible(in.URL) {
			return unauthListing{url: in.URL, failed: uniformLoopbackFailure}
		}
		if hint := wrongTransportHint(cfg, err); hint != "" {
			return unauthListing{refuse: agentadmin.VerbAddMCPServer + " refused: " + hint}
		}
		return unauthListing{url: in.URL, failed: listFailureClass(err)}
	}
	return unauthListing{url: in.URL, tools: listed.names, reads: listed.reads}
}

// afterCredentialStored files a tools approval for each read-pending server
// of ns that uses ns/name, now that it can be reached (plan P4.3 trigger).
func (s *agentAdminService) afterCredentialStored(ctx context.Context, ns, name string) {
	if s.afterCredentialHook != nil {
		s.afterCredentialHook() // test seam (GitHub #314, review-20261013-99c9 F1)
	}
	ref := "secret://" + ns + "/" + name
	for _, p := range s.c.Registry.ListProjects() {
		if got, ok := agentns.FromID(p.ID); !ok || got != ns {
			continue
		}
		for _, srv := range p.MCP.Servers {
			if srv.Auth.ValueFrom != ref {
				continue
			}
			s.fileServerTools(ctx, ns, p.ID, srv.Name, srv.URL, srv.Transport, srv.Auth, p.Permissions.Secrets)
		}
	}
}

// fileServerTools lists one approved, read-pending server and files the
// approval of what it listed. Nothing is filed when the listing fails or
// lists nothing: the server stays read-pending, and the reason is logged.
func (s *agentAdminService) fileServerTools(ctx context.Context, ns, projectID, server, rawURL, transport string, auth mcpauth.Auth, secrets []string) {
	log := s.c.Logger.With().Str("namespace", ns).Str("project", projectID).Str("server", server).Logger()
	a, err := s.grants.GetIntegration(ctx, projectID, server)
	if err != nil || a.RemovedAt != nil || !a.ReadPending {
		return
	}
	cfg, err := s.c.serverConfigFor(projectID, server, rawURL, transport, auth, secrets)
	if err != nil {
		log.Info().Err(err).Msg("agent admin: the server cannot be listed yet")
		return
	}
	listed, err := s.c.listMCPTools(ctx, cfg)
	tools := listed.names
	if hint := wrongTransportHint(cfg, err); hint != "" {
		// Design §6.1 item 4: tools_pending says why (in memory).
		s.setToolGap(projectID, server, hint)
	}
	if err != nil || len(tools) == 0 {
		log.Warn().Err(err).Int("tools", len(tools)).Msg("agent admin: the connected server listed no usable tools; it stays read-pending")
		return
	}
	raw, _ := json.Marshal(agentadmin.ApproveServerToolsInput{Project: projectID, Server: server, Tools: tools, Reads: listed.reads})
	res, err := s.doInternal(ctx, ns, agentadmin.VerbApproveServerTools, raw)
	if err != nil {
		log.Error().Err(err).Msg("agent admin: filing the tools approval failed")
		return
	}
	if res.Effect == EffectRefused && res.WaitingOnLock {
		// Design §19.15 (GitHub #409): recorded by doInternal; filed again
		// once the holding request is decided.
		log.Info().Str("reason", res.Reason).Msg("agent admin: the tools approval waits for another change to be decided")
		s.setToolGap(projectID, server, waitingNote(res.LockedByRequest))
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
		if len(res.Withheld) > 0 {
			s.setToolGap(projectID, server, withheldNote(res.Withheld)) // design §6.4 item 5
		}
		return
	}
	// Design §6.4 item 4: the agent never sees this approval's sentence, so
	// list_my_setup says what was withheld.
	s.setToolGap(projectID, server, withheldNote(res.Withheld))
}

// withheldNote is list_my_setup's note for tools withheld as changes
// (design §6.4, GitHub #405), built from the typed field; "" when none.
func withheldNote(withheld []string) string {
	if len(withheld) == 0 {
		return ""
	}
	return "the server's tools " + strings.Join(withheld, ", ") + " change things and are not approved as reads; " +
		"to propose changes with them, remove the server and add it again naming them in write_tools"
}

// waitingNote is list_my_setup's note for a server whose tools approval
// waits for another change (design §19.15 item 2).
func waitingNote(holder string) string {
	if holder == "" {
		return "its tools approval is filed once the change waiting for approval is decided"
	}
	return "its tools approval is filed once request " + holder + " is decided"
}

// waitingTool is a tools approval refused because a waiting request held a
// lock (design §19.15 item 2): the server, and the request it waits for
// ("" when unknown).
type waitingTool struct {
	ns, project, server, holder string
}

// recordWaitingTools records a refused approve_server_tools whose lock is
// held, or clears the record once one is filed. The caller holds s.mu, in
// the section whose state load saw the lock (design §19.15 item 5).
func (s *agentAdminService) recordWaitingTools(ns string, input json.RawMessage, ch agentadmin.Change, filed bool) {
	var in agentadmin.ApproveServerToolsInput
	if json.Unmarshal(input, &in) != nil {
		return
	}
	key := in.Project + "/" + in.Server
	switch {
	case filed:
		delete(s.waitingTools, key)
	case ch.LockedOn != "":
		if s.waitingTools == nil {
			s.waitingTools = map[string]waitingTool{}
		}
		s.waitingTools[key] = waitingTool{ns: ns, project: in.Project, server: in.Server, holder: ch.LockedByRequest}
	}
}

// fileWaitingTools is the widening_change terminal observer (design §19.15
// item 3, GitHub #409): once r is decided, whichever way, the tools
// approvals that waited for it are filed again, in the background. A
// record for the server r itself was about is dropped: r was that server's
// own tools approval, and the person just answered it.
func (s *agentAdminService) fileWaitingTools(ctx context.Context, r persistence.AgentApprovalRequestRow, _ string) {
	if r.Namespace == "" {
		return
	}
	var pl approvalPayload
	_ = json.Unmarshal(r.Rendered, &pl) // unreadable: no locks, so every record retries
	bg := context.WithoutCancel(ctx)
	s.background(func() {
		s.mu.Lock()
		var due []waitingTool
		for key, w := range s.waitingTools {
			if w.ns != r.Namespace || (w.holder != "" && w.holder != r.ID) {
				continue
			}
			delete(s.waitingTools, key)
			if containsString(pl.Locks, agentadmin.LockIntegration(w.project, w.server)) {
				s.setToolGap(w.project, w.server, "")
				continue
			}
			due = append(due, w)
		}
		s.mu.Unlock()
		sort.Slice(due, func(i, j int) bool { return due[i].project+"/"+due[i].server < due[j].project+"/"+due[j].server })
		for _, w := range due {
			s.refileTools(bg, w)
		}
	})
}

// refileTools files one waiting server's tools approval again, with the
// project and server as the registry has them now; fileServerTools keeps
// its guards (still read-pending, not removed, credential present).
func (s *agentAdminService) refileTools(ctx context.Context, w waitingTool) {
	s.setToolGap(w.project, w.server, "")
	p := s.c.Registry.GetProject(w.project)
	if p != nil {
		if got, ok := agentns.FromID(p.ID); ok && got == w.ns {
			for _, srv := range p.MCP.Servers {
				if srv.Name == w.server {
					s.fileServerTools(ctx, w.ns, p.ID, srv.Name, srv.URL, srv.Transport, srv.Auth, p.Permissions.Secrets)
					return
				}
			}
		}
	}
	// Removed while it waited (review 21b5 F2): nothing to ask for.
	s.c.Logger.Info().Str("namespace", w.ns).Str("project", w.project).Str("server", w.server).
		Msg("agent admin: a server whose tools approval waited is gone; nothing filed")
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
	tools := verb == agentadmin.VerbApproveServerTools
	if ch.Class != agentadmin.Widening {
		res := AgentAdminResult{Effect: EffectRefused, Reason: ch.Reason, MissingTools: ch.MissingTools, Withheld: ch.Withheld}
		if tools && ch.LockedOn != "" {
			s.recordWaitingTools(ns, input, ch, false) // design §19.15 item 2
			res.WaitingOnLock, res.LockedByRequest = true, ch.LockedByRequest
		}
		return res, nil
	}
	p, err := s.fileProposal(ctx, AgentSystemActor, "", ch)
	if err != nil {
		return AgentAdminResult{}, err
	}
	res, err := s.requestApproval(ctx, p, ch)
	res.Withheld = ch.Withheld
	if tools && err == nil {
		s.recordWaitingTools(ns, input, ch, true)
	}
	return res, err
}

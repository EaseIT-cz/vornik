package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// test_connection (agent-administered design §6.2, GitHub #101): probe one
// approved connection with its stored credential and answer with one closed
// code. A read: it changes no configuration, files nothing and needs no
// approval; it reaches only a destination and credential a person approved,
// with the methods that person approved.

const (
	// testConnDefaultTimeout bounds one probe end to end.
	testConnDefaultTimeout = 10 * time.Second
	// testConnPerConnection and testConnPerKey are the rate limits per
	// minute (review 00b7 finding 4).
	testConnPerConnection = 6
	testConnPerKey        = 30
	testConnWindow        = time.Minute
	// testConnAuditTool and testConnAuditTaskPrefix name the probe's audit
	// row. The task id starts with "companion:" so the orphan-row doctor
	// (which deletes any other id that names no task) keeps it (review 00b7
	// finding 3).
	testConnAuditTool       = "agentadmin:test_connection"
	testConnAuditTaskPrefix = "companion:test_connection:"
)

// connLimiter is the in-process sliding window of probe times: per (key,
// project, connection) and per key. It resets on restart, which an agent
// cannot cause.
type connLimiter struct {
	mu    sync.Mutex
	calls map[string][]time.Time
}

// allow records a call and returns "" when it is within both limits, or
// the refusal. A refused call is not recorded.
func (l *connLimiter) allow(key, project, name string, now time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls == nil {
		l.calls = map[string][]time.Time{}
	}
	conn, all := "c:"+key+"/"+project+"/"+name, "k:"+key
	recent := func(k string) []time.Time {
		// kept reuses the slice's backing array: filtering in place is safe
		// because each element is read before its slot is written, and the
		// map entry is replaced with kept below, under l.mu.
		kept := l.calls[k][:0]
		for _, t := range l.calls[k] {
			if now.Sub(t) < testConnWindow {
				kept = append(kept, t)
			}
		}
		l.calls[k] = kept
		return kept
	}
	if len(recent(conn)) >= testConnPerConnection {
		return fmt.Sprintf("%s was tested %d times in the last minute; wait before testing it again", name, testConnPerConnection)
	}
	if len(recent(all)) >= testConnPerKey {
		return fmt.Sprintf("%d tests in the last minute from this assistant; wait", testConnPerKey)
	}
	l.calls[conn] = append(l.calls[conn], now)
	l.calls[all] = append(l.calls[all], now)
	return ""
}

func (s *agentAdminService) testConnTimeoutOrDefault() time.Duration {
	if s.testConnTimeout > 0 {
		return s.testConnTimeout
	}
	return testConnDefaultTimeout
}

func refuseTestConn(format string, args ...any) error {
	return fmt.Errorf(agentadmin.VerbTestConnection+" refused: "+format, args...)
}

// TestConnectionJSON answers test_connection for an agent admin key.
func (s *agentAdminService) TestConnectionJSON(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (any, error) {
	if key == nil || !key.AgentAdmin || !agentns.Valid(key.AgentNamespace) {
		return nil, fmt.Errorf("not an agent admin key")
	}
	ns := key.AgentNamespace
	in, err := agentadmin.ParseTestConnectionInput(ns, raw)
	if err != nil {
		return nil, refuseTestConn("%v", err)
	}
	pid := agentns.ID(ns, in.Project)
	p := s.c.Registry.GetProject(pid)
	if p == nil {
		return nil, refuseTestConn("there is no project %q", pid)
	}
	var srv *registry.MCPServerConfig
	for i := range p.MCP.Servers {
		if p.MCP.Servers[i].Name == in.Name {
			srv = &p.MCP.Servers[i]
		}
	}
	var api *registry.ProjectAPI
	for i := range p.APIs {
		if p.APIs[i].Name == in.Name {
			api = &p.APIs[i]
		}
	}
	switch {
	case srv != nil && api != nil:
		// GitHub #286: add_mcp_server does not yet refuse an API's name.
		return nil, refuseTestConn("%s has both an MCP server and an API named %s; remove one", pid, in.Name)
	case srv == nil && api == nil:
		return nil, refuseTestConn("%s has no server or API named %q; list_my_setup lists them (one awaiting approval is not there until the person approves it)", pid, in.Name)
	}
	var method string
	if api != nil {
		if method = s.safeReadMethod(ctx, pid, *api); method == "" {
			if a, err := s.grants.GetIntegration(ctx, pid, in.Name); err == nil && a.RemovedAt == nil {
				return nil, refuseTestConn("%s has no approved read method (GET or HEAD), and a test never writes", in.Name)
			}
		}
	}
	if why := s.limiter.allow(key.ID, pid, in.Name, s.now()); why != "" {
		return nil, refuseTestConn("%s", why)
	}
	pctx, cancel := context.WithTimeout(ctx, s.testConnTimeoutOrDefault())
	defer cancel()
	var code agentadmin.ConnectionCode
	var class string
	if srv != nil {
		code, class = s.probeMCP(pctx, p, *srv, nil)
	} else {
		code, class = s.probeAPI(pctx, pid, *api, method)
	}
	res := agentadmin.ConnectionResult{Code: code, HTTPStatusClass: class, CheckedAt: s.now().UTC()}
	if a, err := s.grants.GetIntegration(ctx, pid, in.Name); err == nil && a.RemovedAt == nil &&
		(a.StoredAccountLabel() != persistence.AccountLabelDefault || a.IdentitySource != "") {
		res.Account, res.IdentitySource = a.StoredAccountLabel(), a.IdentitySource
	}
	s.auditTestConn(ctx, pid, in.Name, res)
	return res, nil
}

// safeReadMethod is HEAD when the approved read set and the API hold it,
// else GET, else "" (design §6.2: never a write).
func (s *agentAdminService) safeReadMethod(ctx context.Context, pid string, api registry.ProjectAPI) string {
	a, err := s.grants.GetIntegration(ctx, pid, api.Name)
	if err != nil || a.RemovedAt != nil {
		return http.MethodGet // not approved: the client refuses before sending; the code says so
	}
	for _, m := range []string{http.MethodHead, http.MethodGet} {
		if containsString(a.ReadTools, m) && containsString(api.Methods, m) {
			return m
		}
	}
	return ""
}

// liveApproval reports whether the connection's approval is live.
func (s *agentAdminService) liveApproval(ctx context.Context, pid, name string) bool {
	a, err := s.grants.GetIntegration(ctx, pid, name)
	return err == nil && a.RemovedAt == nil
}

// probeMCP runs initialize and tools/list against an approved server with
// its credential, through the SSRF guard (listMCPTools attaches it; review
// 00b7 finding 1). onErr, when set, sees the transport error (tests).
func (s *agentAdminService) probeMCP(ctx context.Context, p *registry.Project, srv registry.MCPServerConfig, onErr func(error)) (agentadmin.ConnectionCode, string) {
	if !s.liveApproval(ctx, p.ID, srv.Name) {
		return agentadmin.ConnNotApproved, ""
	}
	cfg, err := s.c.serverConfigFor(p.ID, srv.Name, srv.URL, srv.Transport, srv.Auth, p.Permissions.Secrets)
	switch {
	case errors.Is(err, errCredentialMissing), errors.Is(err, errServerNotDialled):
		return agentadmin.ConnCredentialMissing, ""
	case err != nil:
		return agentadmin.ConnUnknown, ""
	}
	_, err = s.c.listMCPTools(ctx, cfg)
	if onErr != nil {
		onErr(err)
	}
	if err == nil {
		return agentadmin.ClassifyConnectionError(nil, http.StatusOK)
	}
	status := 0
	var ce *mcp.CallError
	if errors.As(err, &ce) {
		status = ce.Status
	}
	return agentadmin.ClassifyConnectionError(err, status)
}

// probeAPI sends one safe read of the base URL through the role route's own
// client: live approval, approved read set, credential injected, guard, no
// redirect, credential scrubbed. The response is discarded.
func (s *agentAdminService) probeAPI(ctx context.Context, pid string, api registry.ProjectAPI, method string) (agentadmin.ConnectionCode, string) {
	// The approval is checked here, as probeMCP does, not left to the
	// client's "unknown provider" refusal (review 315a F3).
	if !s.liveApproval(ctx, pid, api.Name) {
		return agentadmin.ConnNotApproved, ""
	}
	cl := s.c.agentAPIClient(pid, false, map[string]bool{http.MethodGet: true, http.MethodHead: true})
	if cl == nil {
		return agentadmin.ConnUnknown, ""
	}
	resp, err := cl.Call(ctx, apigateway.Request{Provider: api.Name, Method: method, Path: ""})
	switch {
	case errors.Is(err, apigateway.ErrUnknownProvider):
		return agentadmin.ConnNotApproved, ""
	case errors.Is(err, apigateway.ErrGatewayAuth):
		return agentadmin.ConnCredentialMissing, ""
	case err != nil:
		return agentadmin.ClassifyConnectionError(err, 0)
	}
	return agentadmin.ClassifyConnectionError(nil, resp.Status)
}

// auditTestConn writes the probe's row; the newest one is "last checked"
// in list_my_setup and on the assistants page. A failed write is logged and
// never fails the answer.
func (s *agentAdminService) auditTestConn(ctx context.Context, pid, name string, res agentadmin.ConnectionResult) {
	if s.c.repos == nil || s.c.repos.ToolAudit == nil {
		return
	}
	outcome := "error"
	if res.Code == agentadmin.ConnOK {
		outcome = "ok"
	}
	entry := &persistence.ToolAuditEntry{
		ID: persistence.GenerateID("ta"), ProjectID: pid, TaskID: testConnAuditTaskPrefix + name,
		ExecutionID: persistence.GenerateID("compex"), ToolName: testConnAuditTool,
		ToolInput:  "connection=" + name,
		ToolOutput: fmt.Sprintf("code=%s http_status_class=%s", res.Code, res.HTTPStatusClass),
		Outcome:    outcome, CreatedAt: res.CheckedAt,
	}
	if err := s.c.repos.ToolAudit.Log(ctx, entry); err != nil {
		s.c.Logger.Warn().Err(err).Str("project", pid).Str("connection", name).Msg("agent admin: test_connection audit write failed")
	}
}

// connectionCheckOf reads the newest probe of a connection: one indexed
// LIMIT 1 per connection (a grouped read would need new SQL, design §6.2).
// tool_audit_log keeps 30 days, so "never" means none on record.
func (s *agentAdminService) connectionCheckOf(ctx context.Context, pid, name string) connectionCheck {
	never := connectionCheck{LastResult: "never", CheckNote: neverCheckedNote}
	if s.c.repos == nil || s.c.repos.ToolAudit == nil {
		return never
	}
	task, tool := testConnAuditTaskPrefix+name, testConnAuditTool
	rows, err := s.c.repos.ToolAudit.List(ctx, persistence.ToolAuditFilter{ProjectID: &pid, TaskID: &task, ToolName: &tool, PageSize: 1})
	if err != nil || len(rows) == 0 {
		return never
	}
	code, _, _ := strings.Cut(strings.TrimPrefix(rows[0].ToolOutput, "code="), " ")
	if code == "" {
		return never
	}
	at := rows[0].CreatedAt.UTC()
	return connectionCheck{LastCheckedAt: &at, LastResult: code}
}

func (p agentAdminProxy) TestConnectionJSON(ctx context.Context, key *persistence.APIKey, input json.RawMessage) (any, error) {
	s := p.c.agentAdmin()
	if s == nil {
		return nil, agentadmin.ErrUnavailable
	}
	return s.TestConnectionJSON(ctx, key, input)
}

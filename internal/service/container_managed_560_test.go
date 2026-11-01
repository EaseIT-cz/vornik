package service

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/listener"
	"vornik.io/vornik/internal/mcp"
)

// GitHub #560 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1a)", A1 and L5): the dedicated metrics server never authenticates,
// so a managed instance never starts it, whatever metrics.addr says; metrics
// are scraped on the operations listener behind metrics.require_admin.
func TestDedicatedMetricsServer560(t *testing.T) {
	self := config.DefaultConfig()
	self.Metrics.Enabled = true
	if !dedicatedMetricsServer(self) {
		t.Fatal("self-hosted with metrics enabled must start the dedicated metrics server")
	}
	if got := buildObservabilityConfig(self).MetricsAddr; got != self.Metrics.Addr {
		t.Fatalf("self-hosted MetricsAddr = %q, want %q", got, self.Metrics.Addr)
	}

	managed := config.DefaultConfig()
	managed.Metrics.Enabled = true
	managed.Hosting.Mode = config.HostingModeManaged
	if dedicatedMetricsServer(managed) {
		t.Fatal("a managed instance started the unauthenticated dedicated metrics server")
	}
	if got := buildObservabilityConfig(managed).MetricsAddr; got != "" {
		t.Fatalf("managed MetricsAddr = %q, want empty", got)
	}

	off := config.DefaultConfig()
	if dedicatedMetricsServer(off) {
		t.Fatal("metrics disabled must not start the dedicated metrics server")
	}
}

type otherSessionBackend struct{}

func (otherSessionBackend) Name() string { return "session" }
func (otherSessionBackend) Authenticate(context.Context, auth.Credential) (*auth.Identity, error) {
	return nil, auth.ErrNoCredential
}

// A4b: in managed hosting every session backend carries the listener rule,
// and one that cannot is not wired at all (fail closed).
func TestManagedSessionRule560(t *testing.T) {
	managed := &Container{Config: config.DefaultConfig(), Logger: zerolog.Nop()}
	managed.Config.Hosting.Mode = config.HostingModeManaged
	sb := authsession.NewSessionBackend(nil, nil, 0)
	if !managed.applyManagedSessionRule(sb) || sb.DemoteAdmin == nil {
		t.Fatal("managed hosting must set the listener rule on the session backend")
	}
	if !sb.DemoteAdmin(listener.With(context.Background(), listener.Public)) ||
		sb.DemoteAdmin(listener.With(context.Background(), listener.Operations)) {
		t.Fatal("the rule must demote everywhere but the operations listener")
	}
	if managed.applyManagedSessionRule(otherSessionBackend{}) {
		t.Fatal("a session backend that cannot carry the rule was accepted in managed hosting")
	}

	self := &Container{Config: config.DefaultConfig(), Logger: zerolog.Nop()}
	sb2 := authsession.NewSessionBackend(nil, nil, 0)
	if !self.applyManagedSessionRule(sb2) || sb2.DemoteAdmin != nil || !self.applyManagedSessionRule(otherSessionBackend{}) {
		t.Fatal("self-hosted must leave session backends unchanged")
	}
}

// serve runs srv on a loopback port and returns its base URL.
func serve(t *testing.T, srv *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

const instanceAdmin = "sk-instance-admin-560"

// probeMux answers /api/v1/probe with the listener stamp and whether the
// presented key is admin-class, and /metrics with 200.
func probeMux(adm config.AdminConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/probe", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("X-Listener", string(listener.FromContext(r.Context())))
		w.Header().Set("X-Admin", strconv.FormatBool(adm.IsAdminKey(key)))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func get(t *testing.T, url, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func identityMW(h http.Handler) http.Handler { return h }

// Invariants 1 and 2 at the listener level, through the production
// constructors: on the public listener an admin key is refused and /metrics
// is gone; on the operations listener the same key works and /metrics is
// served; each server stamps its own listener.
func TestManagedListeners560(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Hosting.Mode = config.HostingModeManaged
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{instanceAdmin}}
	c := &Container{Config: cfg, Logger: zerolog.Nop()}
	mux := probeMux(cfg.Admin)

	pub := serve(t, c.newPublicServer(mux, time.Second, time.Second))
	ops := serve(t, newOperationsServer("", mux, identityMW, time.Second, time.Second))

	if r := get(t, pub+"/api/v1/probe", instanceAdmin); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin key on the public listener: %d, want 401", r.StatusCode)
	}
	if r := get(t, pub+"/api/v1/probe", "sk-vornik-project"); r.StatusCode != http.StatusNoContent || r.Header.Get("X-Listener") != string(listener.Public) {
		t.Fatalf("project key on the public listener: %d, listener %q", r.StatusCode, r.Header.Get("X-Listener"))
	}
	if r := get(t, pub+"/metrics", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("/metrics on the public listener: %d, want 404", r.StatusCode)
	}
	if r := get(t, ops+"/api/v1/probe", instanceAdmin); r.StatusCode != http.StatusNoContent || r.Header.Get("X-Admin") != "true" || r.Header.Get("X-Listener") != string(listener.Operations) {
		t.Fatalf("admin key on the operations listener: %d, admin %q, listener %q", r.StatusCode, r.Header.Get("X-Admin"), r.Header.Get("X-Listener"))
	}
	if r := get(t, ops+"/metrics", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("/metrics on the operations listener: %d, want 200", r.StatusCode)
	}
}

// A regression guard (passes before and after): self-hosted, the admin key
// works on the TCP root and /metrics is served there.
func TestSelfMode560_Unchanged(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{instanceAdmin}}
	c := &Container{Config: cfg, Logger: zerolog.Nop()}
	pub := serve(t, c.newPublicServer(probeMux(cfg.Admin), time.Second, time.Second))
	if r := get(t, pub+"/api/v1/probe", instanceAdmin); r.StatusCode != http.StatusNoContent || r.Header.Get("X-Admin") != "true" {
		t.Fatalf("self-hosted admin key: %d, admin %q", r.StatusCode, r.Header.Get("X-Admin"))
	}
	if r := get(t, pub+"/metrics", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("self-hosted /metrics: %d", r.StatusCode)
	}
}

// A4: in managed hosting the agent socket's server carries the listener gate
// too, through the production constructor: an admin key is refused in every
// form; an agent's own key passes.
func TestAgentSocketGate560(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Hosting.Mode = config.HostingModeManaged
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{instanceAdmin}}
	c := &Container{Config: cfg, Logger: zerolog.Nop()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Listener", string(listener.FromContext(r.Context())))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := newAgentSocketServer(mux, identityMW, c.listenerGate(), time.Second, time.Second)
	sock := serve(t, srv)
	path := sock + "/api/v1/internal/llm-usage" // an agent route (api.IsAgentRoute)
	for name, set := range map[string]func(http.Header){
		"X-API-Key": func(h http.Header) { h.Set("X-API-Key", instanceAdmin) },
		"Bearer":    func(h http.Header) { h.Set("Authorization", "Bearer "+instanceAdmin) },
		"Basic": func(h http.Header) {
			h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("api:"+instanceAdmin)))
		},
	} {
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		set(req.Header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("admin key (%s) on the agent socket: %d, want 401", name, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer sk-vornik-agent-task-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("X-Listener") != string(listener.AgentSocket) {
		t.Fatalf("agent key on the agent socket: %d, listener %q", resp.StatusCode, resp.Header.Get("X-Listener"))
	}
}

// M1c D2 and D5: a managed instance mints no stdio MCP program, and a
// configured HTTP MCP server gets a guarded client (no loopback, no private,
// same-host redirects); self-hosted, both are unchanged.
func TestManagedMCP560_NoStdioAndGuardedRuntime(t *testing.T) {
	managed := &Container{Config: config.DefaultConfig(), Logger: zerolog.Nop()}
	managed.Config.Hosting.Mode = config.HostingModeManaged
	self := &Container{Config: config.DefaultConfig(), Logger: zerolog.Nop()}
	stdio := mcp.ServerConfig{Name: "s", Transport: "stdio", Command: "uvx"}
	if !managed.configuredMCPProgram(stdio, "project", "p1").IsZero() {
		t.Fatal("managed hosting minted a stdio MCP program")
	}
	if self.configuredMCPProgram(stdio, "project", "p1").IsZero() {
		t.Fatal("self-hosted stopped minting a stdio MCP program")
	}

	httpCfg := mcp.ServerConfig{Name: "h", Transport: "streamable-http", URL: "http://127.0.0.1:5432/mcp"}
	selfCfg := httpCfg
	self.guardManagedMCPServer(&selfCfg, false)
	if selfCfg.HTTPClient != nil {
		t.Fatal("self-hosted: a configured MCP server's client changed")
	}
	dialguard.SetManagedPolicy(&dialguard.ManagedPolicy{})
	t.Cleanup(func() { dialguard.SetManagedPolicy(nil) })
	managed.guardManagedMCPServer(&httpCfg, false)
	if httpCfg.HTTPClient == nil {
		t.Fatal("managed: a configured HTTP MCP server kept the unguarded default client")
	}
	if _, err := httpCfg.HTTPClient.Get("http://127.0.0.1:5432/mcp"); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("managed: the runtime MCP client dialled loopback: %v", err)
	}
	if _, err := httpCfg.HTTPClient.Get("http://10.0.0.5:5432/mcp"); !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("managed: the runtime MCP client dialled a private address: %v", err)
	}
	preset := mcp.ServerConfig{Name: "gh", Transport: "streamable-http", URL: "https://api.github.com", HTTPClient: http.DefaultClient}
	managed.guardManagedMCPServer(&preset, false)
	if preset.HTTPClient != http.DefaultClient {
		t.Fatal("an integration's own client was replaced")
	}
}

// M1c D0: the container sets the outbound policy from its config.
func TestManagedPolicy560_SetFromConfig(t *testing.T) {
	t.Cleanup(func() { dialguard.SetManagedPolicy(nil) })
	cfg := config.DefaultConfig()
	cfg.Hosting.Mode = config.HostingModeManaged
	cfg.Hosting.ModelHosts = []string{"gateway.internal"}
	if p := dialguard.ManagedPolicyFor(cfg.Hosting.Managed(), cfg.Hosting.ModelHosts); p == nil || len(p.ModelHosts) != 1 {
		t.Fatalf("managed config: policy %v", p)
	}
	if dialguard.ManagedPolicyFor(false, nil) != nil {
		t.Fatal("a self-hosted config implies a managed policy")
	}
}

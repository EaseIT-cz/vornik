package service

// GitHub #429 (2026-10-17): the admin MCP refresh hand-built its server
// configs. adminMCPRefresher.RefreshAll rebuilt every project's
// mcp.ServerConfig from the registry's fields and called StartForProject,
// skipping what the boot/reload builder (mcpDesiredServers) does: the
// agent-namespace SSRF dial guard, the auth: block, withholding and
// daemon inheritance. These tests drive the refresher the admin page is
// wired with, against a real mcp.Manager and real HTTP MCP servers
// (agent-administered design §6.6).

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/registry"
)

// writeRefreshFixture loads a registry holding one project with the given
// id and body, plus a swarm and workflow in the project's namespace (an
// agent project may reference nothing outside it).
func writeRefreshFixture(t *testing.T, projectID, swarmID, workflowID, body string) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"swarms/s.md":     "---\nswarmId: \"" + swarmID + "\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n",
		"workflows/w.md":  "---\nworkflowId: \"" + workflowID + "\"\nentrypoint: \"step1\"\nsteps:\n  step1:\n    type: \"agent\"\n    prompt: \"do work\"\n    role: \"coder\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n",
		"projects/p.yaml": "projectId: \"" + projectID + "\"\nswarmId: \"" + swarmID + "\"\ndefaultWorkflowId: \"" + workflowID + "\"\n" + body,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reg.GetProject(projectID) == nil {
		t.Fatalf("the fixture project %q did not load", projectID)
	}
	return reg
}

func refreshContainer(t *testing.T, reg *registry.Registry, daemonServers ...config.MCPServerConfig) *Container {
	t.Helper()
	c := &Container{
		Logger:     zerolog.Nop(),
		Registry:   reg,
		Config:     &config.Config{MCP: config.MCPConfig{Servers: daemonServers}},
		mcpManager: mcp.NewManager(zerolog.Nop()),
	}
	t.Cleanup(func() { c.mcpManager.Close() })
	return c
}

func refreshAll(t *testing.T, c *Container) {
	t.Helper()
	r := c.newAdminMCPRefresher()
	if r == nil {
		t.Fatal("the container wired no admin MCP refresher")
	}
	if err := r.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}
}

// An agent project's server whose URL the SSRF dial guard refuses is not
// dialled by a refresh. The host "localhost." resolves to loopback but is
// not the literal localhost agentDialGuard allow-lists, so the guard judges
// the resolved address and refuses it before any byte is sent, while an
// unguarded client reaches the fake server on 127.0.0.1.
func TestAdminMCPRefresh_AgentServerRefusedByTheGuardIsNotDialled(t *testing.T) {
	addrs, err := net.LookupHost("localhost.")
	if err != nil || len(addrs) == 0 {
		t.Skipf("this host does not resolve localhost. (%v); the test needs a loopback name the guard does not allow-list", err)
	}
	fake := newFakeMCP(t, "", "fetch_page")
	u, _ := url.Parse(fake.srv.URL)
	guarded := "http://localhost.:" + u.Port() + "/mcp"

	reg := writeRefreshFixture(t, "hermes--venues", "hermes--team", "hermes--flow", `broker: true
mcp:
  servers:
    - name: "scraper"
      transport: "streamable-http"
      url: "`+guarded+`"
      allowed_tools: ["fetch_page"]
`)
	c := refreshContainer(t, reg)
	refreshAll(t, c)

	if got := fake.seen(); len(got) != 0 {
		t.Fatalf("a refresh dialled an agent server the SSRF guard refuses: %d request(s) reached it", len(got))
	}
	if tools := c.mcpManager.Tools("hermes--venues"); len(tools) != 0 {
		t.Fatalf("a refresh installed tools from a guarded URL: %v", tools)
	}
	// The refused dial leaves the server pending with the config the refresh
	// handed over; the dial on use (and the reconnector) retries THAT config,
	// so it must be the guarded one too.
	if _, err := c.mcpManager.Execute(context.Background(), "hermes--venues", "mcp__scraper__fetch_page", "{}"); err == nil {
		t.Fatal("a call to a server the guard refuses succeeded")
	}
	if got := fake.seen(); len(got) != 0 {
		t.Fatalf("the dial on use of a refresh-recorded pending server bypassed the SSRF guard: %d request(s)", len(got))
	}
}

// An authenticated server keeps its credential across a refresh: every
// request it sees carries the resolved header, and the project holds its
// tool afterwards.
func TestAdminMCPRefresh_AuthenticatedServerKeepsItsCredential(t *testing.T) {
	t.Setenv("W15U_MCP_TOKEN", "tok-429")
	fake := newFakeMCP(t, "Bearer tok-429", "search")
	reg := writeRefreshFixture(t, "test-project", "test-swarm", "test-workflow", `permissions:
  secrets: ["W15U_MCP_TOKEN"]
mcp:
  servers:
    - name: "n8n"
      transport: "streamable-http"
      url: "`+fake.srv.URL+`"
      auth:
        mode: static
        value_from: "secret://W15U_MCP_TOKEN"
        value_prefix: "Bearer "
`)
	c := refreshContainer(t, reg)
	refreshAll(t, c)

	seen := fake.seen()
	if len(seen) == 0 {
		t.Fatal("the refresh never dialled the authenticated server")
	}
	for i, a := range seen {
		if a != "Bearer tok-429" {
			t.Fatalf("request %d of the refresh carried Authorization %q, want the configured credential", i, a)
		}
	}
	if tools := c.mcpManager.Tools("test-project"); len(tools) != 1 {
		t.Fatalf("after a refresh the authenticated server holds %d tool(s), want 1", len(tools))
	}
}

// A name-only subscription is dialled at the daemon-scope server it names,
// as at a reload, not with an empty transport.
func TestAdminMCPRefresh_NameOnlyEntryInheritsTheDaemonServer(t *testing.T) {
	fake := newFakeMCP(t, "", "lookup")
	reg := writeRefreshFixture(t, "test-project", "test-swarm", "test-workflow", `mcp:
  servers:
    - name: "shared"
`)
	c := refreshContainer(t, reg, config.MCPServerConfig{Name: "shared", Transport: "streamable-http", URL: fake.srv.URL})
	refreshAll(t, c)

	if len(fake.seen()) == 0 {
		t.Fatal("a refresh did not dial the daemon server a name-only entry subscribes to")
	}
	if tools := c.mcpManager.Tools("test-project"); len(tools) != 1 {
		t.Fatalf("after a refresh the name-only entry holds %d tool(s), want 1", len(tools))
	}
}

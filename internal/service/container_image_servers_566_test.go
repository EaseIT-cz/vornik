package service

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// GitHub #566, SaaS MVP M7 (18-security-and-tenancy.md, "Amendment
// 2026-10-28 (#566, M7 daemon half)", E3): only a URL taken from config.yaml
// reaches an image server; the same URL typed into a project file does not.
// Found at: design review; regression test at: integration (the container's
// real wiring over a loaded registry and a listener on loopback).

func imageServerFixture(t *testing.T) (*Container, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	mcpURL := srv.URL + "/mcp"
	reg := writeMCPInheritFixture(t, "mcp:\n  servers:\n    - name: \"gws\"\n    - name: \"copy\"\n      transport: \"streamable-http\"\n      url: \""+mcpURL+"\"\n")
	cfg := config.DefaultConfig()
	cfg.Hosting.Mode = config.HostingModeManaged
	cfg.Hosting.ImageServers = []string{u.Host}
	cfg.MCP.Servers = []config.MCPServerConfig{{Name: "gws", Transport: "streamable-http", URL: mcpURL}}
	c := &Container{Logger: zerolog.Nop(), Registry: reg, Config: cfg}
	dialguard.SetManagedPolicy(&dialguard.ManagedPolicy{ImageServers: []netip.AddrPort{netip.MustParseAddrPort(u.Host)}})
	t.Cleanup(func() { dialguard.SetManagedPolicy(nil) })
	return c, mcpURL
}

func reaches(t *testing.T, c *http.Client, raw string) bool {
	t.Helper()
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Get(raw)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func TestManagedMCP566_OnlyConfigURLsReachImageServers(t *testing.T) {
	c, mcpURL := imageServerFixture(t)
	for _, s := range c.daemonMCPServerConfigs() {
		if s.Name == "gws" && !reaches(t, s.HTTPClient, mcpURL) {
			t.Fatal("the daemon-level image server is not reachable")
		}
	}
	byName := map[string]*http.Client{}
	for _, s := range c.mcpDesiredServers()["test-project"] {
		byName[s.Name] = s.HTTPClient
	}
	if !reaches(t, byName["gws"], mcpURL) {
		t.Error("a name-only subscription to the image server is not reachable")
	}
	if byName["copy"] == nil || reaches(t, byName["copy"], mcpURL) {
		t.Error("a project-file entry with the image server's URL reached it")
	}
}

func TestMCPOAuth566_PerServerClientFollowsOrigin(t *testing.T) {
	c, mcpURL := imageServerFixture(t)
	_, perServer := c.mcpOAuthClients()
	daemonRef, ok := c.mcpServerRef("", "gws")
	if !ok || !daemonRef.FromDaemonConfig {
		t.Fatalf("the daemon-level ref: %+v, want FromDaemonConfig", daemonRef)
	}
	nameOnly, ok := c.mcpServerRef("test-project", "gws")
	if !ok || !nameOnly.FromDaemonConfig {
		t.Fatalf("the name-only ref: %+v, want FromDaemonConfig", nameOnly)
	}
	typed, ok := c.mcpServerRef("test-project", "copy")
	if !ok || typed.FromDaemonConfig {
		t.Fatalf("the project-file ref: %+v, want FromDaemonConfig false", typed)
	}
	for name, ref := range map[string]mcpconnect.ServerRef{"daemon-level": daemonRef, "name-only": nameOnly} {
		if !reaches(t, perServer(ref), mcpURL) {
			t.Errorf("%s: the OAuth client cannot reach the image server", name)
		}
	}
	// An agent project's name-only ref takes the image client too, ahead of
	// the agent branch.
	agent := nameOnly
	agent.ProjectID = "hermes--mail"
	if !reaches(t, perServer(agent), mcpURL) {
		t.Error("an agent project's name-only ref took the agent branch, not the image client")
	}
	if reaches(t, perServer(typed), mcpURL) {
		t.Error("the project-file ref's OAuth client reached the image server")
	}
}

// E3 / C8: in managed hosting an agent project's add_api calls dial through
// the customer guard, and no Kong client is built for them: a loopback or
// private base URL is refused, even a listed image pair.
func TestManagedAddAPI566_GuardedAndKongless(t *testing.T) {
	c, mcpURL := imageServerFixture(t)
	c.repos = &storage.Repositories{AgentGrants: struct {
		persistence.AgentGrantRepository
	}{}}
	cl := c.agentAPIClient("hermes--mail", false, map[string]bool{http.MethodGet: true})
	if cl == nil || cl.HTTP == nil {
		t.Fatal("no agent API client in managed hosting")
	}
	for _, base := range []string{mcpURL, "http://10.0.0.5:8080", "http://169.254.169.254"} {
		if reaches(t, cl.HTTP(base), base) {
			t.Errorf("the agent API client dialled %s", base)
		}
	}
}

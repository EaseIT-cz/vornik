package service

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// GitHub #91 (2026-10-08): an oauth server with no grant was dialled and
// 401'd. On every start the daemon logged "connecting unauthenticated", then
// dialled; initialize came back 401 invalid_token and the client logged a
// WARN that reads as a broken credential, with no remedy, again on every
// reconnect and registry refresh. Auth design §14: no grant, no dial; one
// WARN naming `vornikctl mcp connect`, once until the state changes.

// countingMCP counts every request and answers 401, as a vendor does to an
// unauthenticated initialize.
func countingMCP(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range strings.Split(s.b.String(), "\n") {
		if strings.Contains(l, `"level":"warn"`) && strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func noGrantContainer(t *testing.T, projectYAML string, daemon []config.MCPServerConfig) (*Container, *stubTokens, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	tokens := &stubTokens{}
	c := &Container{
		Logger:   zerolog.New(logs),
		Registry: writeMCPInheritFixture(t, projectYAML),
		Config:   &config.Config{MCP: config.MCPConfig{Servers: daemon}},
		repos:    &storage.Repositories{MCPOAuthTokens: tokens},
	}
	return c, tokens, logs
}

func TestOAuthServerWithNoGrant_ProjectScope_NotDialledOneWarn(t *testing.T) {
	srv, hits := countingMCP(t)
	c, tokens, logs := noGrantContainer(t, `mcp:
  servers:
    - name: "atlassian"
      transport: "streamable-http"
      url: "`+srv.URL+`"
      auth:
        mode: oauth
`, nil)

	c.initMCP()
	c.initMCP() // a reload
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests reached an oauth server with no grant", n)
	}
	if n := logs.count("vornikctl mcp connect atlassian -p test-project"); n != 1 {
		t.Fatalf("want exactly one warn naming the remedy, got %d\n%s", n, logs.b.String())
	}
	if n := logs.count("server returned error status"); n != 0 {
		t.Fatalf("the misleading 401 warn is still logged:\n%s", logs.b.String())
	}
	if got := c.mcpNoGrant(); len(got) != 1 || got[0].ProjectID != "test-project" || got[0].Server != "atlassian" || got[0].CredentialScope != "test-project" {
		t.Fatalf("doctor source = %+v", got)
	}
	if got := c.mcpWithheldServers(); len(got) != 0 {
		t.Fatalf("a no-grant server is not a configuration error: %v", got)
	}

	// A grant appears: the next wiring dials it and it leaves the source.
	exp := time.Now().Add(time.Hour)
	tokens.tok = &persistence.MCPOAuthToken{ProjectID: "test-project", ServerName: "atlassian", AccessToken: "t", RefreshToken: "r",
		Resource: srv.URL, ExpiresAt: &exp}
	c.initMCP()
	if hits.Load() == 0 {
		t.Fatal("the server was not dialled once a grant was stored")
	}
	if got := c.mcpNoGrant(); len(got) != 0 {
		t.Fatalf("a connected server is still reported: %+v", got)
	}

	// Disconnect (a state change) warns again (review 5fe0 F5).
	tokens.tok = nil
	c.mcpConnector().InvalidateToken("test-project", "atlassian")
	c.initMCP()
	if n := logs.count("vornikctl mcp connect atlassian -p test-project"); n != 2 {
		t.Fatalf("a disconnect did not warn again: %d warns", n)
	}
}

func TestOAuthServerWithNoGrant_DaemonScope_ListedNotDialled(t *testing.T) {
	srv, hits := countingMCP(t)
	daemon := []config.MCPServerConfig{{Name: "atlassian", Transport: "streamable-http", URL: srv.URL, Auth: mcpauth.Auth{Mode: "oauth"}}}
	c, _, logs := noGrantContainer(t, "", daemon)

	c.refreshMCPRegistry()
	c.refreshMCPRegistry()
	time.Sleep(100 * time.Millisecond) // the registry's async first refresh
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests reached a daemon oauth server with no grant", n)
	}
	if n := logs.count("(run: vornikctl mcp connect atlassian)"); n != 1 {
		t.Fatalf("want one warn naming the daemon-scope remedy (no -p), got %d\n%s", n, logs.b.String())
	}
	snap := c.mcpRegistry.Snapshot(t.Context())
	if len(snap) != 1 || !snap[0].NotDialled || !strings.Contains(snap[0].Error, "vornikctl mcp connect atlassian") {
		t.Fatalf("snapshot = %+v", snap)
	}
	if got := c.mcpNoGrant(); len(got) != 1 || got[0].CredentialScope != "" || got[0].Server != "atlassian" {
		t.Fatalf("doctor source = %+v", got)
	}

	// Removed from config and added back at another URL: warns again (F5).
	c.publishDaemonMCPServers(nil)
	c.refreshMCPRegistry()
	srv2, _ := countingMCP(t)
	c.publishDaemonMCPServers([]config.MCPServerConfig{{Name: "atlassian", Transport: "streamable-http", URL: srv2.URL, Auth: mcpauth.Auth{Mode: "oauth"}}})
	c.refreshMCPRegistry()
	if n := logs.count("(run: vornikctl mcp connect atlassian)"); n != 2 {
		t.Fatalf("a re-added server did not warn again: %d", n)
	}
}

// Review 030f B2: a project's name-only subscription to a daemon oauth server
// shares the daemon pass's key, so the two passes warn once between them,
// removing the daemon server produces no spurious warning, and adding it
// back warns once more.
func TestOAuthServerWithNoGrant_NameOnlySubscriptionWarnsOnce(t *testing.T) {
	srv, hits := countingMCP(t)
	daemon := []config.MCPServerConfig{{Name: "atlassian", Transport: "streamable-http", URL: srv.URL, Auth: mcpauth.Auth{Mode: "oauth"}}}
	c, _, logs := noGrantContainer(t, `mcp:
  servers:
    - name: "atlassian"
`, daemon)
	const remedy = "(run: vornikctl mcp connect atlassian)"
	c.initMCP()
	c.refreshMCPRegistry()
	c.initMCP()
	time.Sleep(100 * time.Millisecond)
	if n := logs.count(remedy); n != 1 {
		t.Fatalf("want one warn across both passes, got %d\n%s", n, logs.b.String())
	}
	c.publishDaemonMCPServers(nil)
	c.initMCP()
	c.refreshMCPRegistry()
	if n := logs.count(remedy); n != 1 {
		t.Fatalf("removing the daemon server warned again: %d", n)
	}
	c.publishDaemonMCPServers(daemon)
	c.initMCP()
	c.refreshMCPRegistry()
	if n := logs.count(remedy); n != 2 {
		t.Fatalf("adding it back did not warn once more: %d", n)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests reached the server", n)
	}
}

package ui

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
)

// Process-spawn law, S1a (https://docs.vornik.io).
// Incident: POST /ui/admin/control-plane/mcp/probe passed the `command` form
// field straight to exec.Command, so an admin session could run any program on
// the daemon host; the add form did the same one reload later.

func TestMCPProbe_RefusesStdioBeforeAnySpawn(t *testing.T) {
	orig := mcpProbeConnect
	defer func() { mcpProbeConnect = orig }()
	mcpProbeConnect = func(context.Context, mcp.ServerConfig, zerolog.Logger) (mcpProbeConn, error) {
		t.Fatal("a stdio probe must never reach connect: that is the spawn")
		return nil, nil
	}
	rec := httptest.NewRecorder()
	NewServer().AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"stdio"}, "command": {"/bin/sh"},
	}))
	if !strings.Contains(rec.Body.String(), "Invalid endpoint") {
		t.Fatalf("expected a refusal, got: %s", rec.Body.String())
	}
}

func TestMCPAdd_RefusesStdio(t *testing.T) {
	s, repo := mcpTestServer(t)
	rec := postMCP(t, s, url.Values{"action": {"add"}, "name": {"files"}, "transport": {"stdio"}, "command": {"/usr/bin/files-mcp"}})
	if !strings.Contains(rec.Header().Get("Location"), "mcp-stdio-refused") {
		t.Fatalf("a stdio server must be refused, got %s", rec.Header().Get("Location"))
	}
	if n := draftCountUI(t, repo); n != 0 {
		t.Fatalf("no proposal may be drafted for a stdio server, got %d", n)
	}
}

// Every UI writer of a config file goes through writeProjectConfigAtomic.
func TestWriteProjectConfigAtomic_RefusesAStdioMCPServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(path, []byte("id: p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdio := "id: p\nmcp:\n  servers:\n    - name: x\n      transport: stdio\n      command: /bin/sh\n"
	if _, err := writeProjectConfigAtomic(path, []byte(stdio)); !errors.Is(err, config.ErrStdioMCPChange) {
		t.Fatalf("err = %v, want ErrStdioMCPChange", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "id: p\n" {
		t.Fatal("a refused write must leave the file untouched")
	}
	// A non-MCP edit still writes.
	if _, err := writeProjectConfigAtomic(path, []byte("id: p\ndescription: ok\n")); err != nil {
		t.Fatalf("an ordinary edit must write: %v", err)
	}
}

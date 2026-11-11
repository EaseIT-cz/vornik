package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #589 (25-claude-desktop-companion.md). Found at production (a field
// report: a companion key in Claude Desktop needed a hand-written Python
// bridge, Node was missing for npx, and the GUI app saw no shell env). A real
// Claude Desktop cannot run in a test, so these drive the commands against a
// fake companion endpoint in a temp HOME, the stand-in the agent connect path
// uses.

const testCompanionKey = "sk-vornik-companion-t.abcdefghijklmnopqrstuvwxyz012345"

// fakeCompanion answers initialize, tools/list and whoami; admin adds the
// admin verb an agent-admin key is offered.
func fakeCompanion(t *testing.T, admin bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testCompanionKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "s1")
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "vornik"}}
		case "tools/list":
			tools := []map[string]any{{"name": "recall"}, {"name": "whoami"}}
			if admin {
				tools = append(tools, map[string]any{"name": "describe_installation"})
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": `{"project_id":"companion-t","effective_repo_scope":"github.com/o/r"}`}}}
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
}

// userConfigDir is os.UserConfigDir under the test's HOME, the directory the
// CLI resolves: $XDG_CONFIG_HOME on Linux, ~/Library/Application Support on
// macOS. Building the Linux path by hand failed the public CE repository's
// macOS lane (2026-11-09) and let the absent-key check pass vacuously there.
func userConfigDir(t *testing.T) string {
	t.Helper()
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// desktopHome points HOME and the config dir at a temp dir holding a Claude
// Desktop config with another server, and returns the config path.
func desktopHome(t *testing.T) (home, cfgPath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("VORNIK_COMPANION_TOKEN", "")
	t.Setenv("VORNIK_URL", "")
	t.Setenv("VORNIK_API_URL", "")
	cfgPath = filepath.Join(userConfigDir(t), "Claude", "claude_desktop_config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"mcpServers":{"other":{"command":"/bin/true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	companionDeskName, companionDeskKeyStdin = "default", false
	return home, cfgPath
}

func TestCompanionConnect_589_ClaudeDesktop(t *testing.T) {
	home, cfgPath := desktopHome(t)
	srv := fakeCompanion(t, false)
	defer srv.Close()
	companionDeskURL = srv.URL
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	var out bytes.Buffer
	if err := runCompanionConnect(context.Background(), "claude-desktop", nil, &out); err != nil {
		t.Fatalf("connect: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "project companion-t") || strings.Contains(out.String(), testCompanionKey) {
		t.Fatalf("output must name the project and never the key:\n%s", out.String())
	}
	raw, _ := os.ReadFile(cfgPath)
	var cfg struct {
		MCPServers map[string]desktopServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	e, ok := cfg.MCPServers["vornik-companion-default"]
	if !ok || cfg.MCPServers["other"].Command != "/bin/true" {
		t.Fatalf("the entry is added and the other server kept: %s", raw)
	}
	if strings.Join(e.Args, " ") != "companion mcp-bridge --name default --url "+srv.URL {
		t.Fatalf("entry args %v", e.Args)
	}
	keyPath := filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")
	fi, err := os.Stat(keyPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v mode %v", err, fi)
	}
	// No file under HOME but the key file holds the key.
	_ = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || p == keyPath {
			return nil
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(testCompanionKey)) {
			t.Errorf("%s holds the key", p)
		}
		return nil
	})

	// The bridge, with no environment key, reads the file and relays.
	t.Setenv("VORNIK_COMPANION_TOKEN", "")
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall","arguments":{"query":"x"}}}` + "\n")
	var bout, berr bytes.Buffer
	if err := runCompanionMCPBridge(context.Background(), in, &bout, &berr); err != nil {
		t.Fatalf("bridge: %v %s", err, berr.String())
	}
	if !strings.Contains(bout.String(), `"id":1`) || !strings.Contains(bout.String(), "companion-t") {
		t.Fatalf("bridge output:\n%s", bout.String())
	}
	if strings.Contains(bout.String()+berr.String(), testCompanionKey) {
		t.Fatal("the bridge printed the key")
	}

	// Disconnect removes the entry and the key file, and keeps the other server.
	out.Reset()
	if err := runCompanionDisconnect("claude-desktop", &out); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "vornik-companion-default") || !strings.Contains(string(raw), `"other"`) {
		t.Fatalf("after disconnect: %s", raw)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("the key file must be gone")
	}
}

func TestCompanionConnect_589_RefusesAnAgentKey(t *testing.T) {
	_, cfgPath := desktopHome(t)
	srv := fakeCompanion(t, true)
	defer srv.Close()
	companionDeskURL = srv.URL
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	err := runCompanionConnect(context.Background(), "claude-desktop", nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "vornikctl agent connect") {
		t.Fatalf("an agent-admin key must be refused, got %v", err)
	}
	if raw, _ := os.ReadFile(cfgPath); strings.Contains(string(raw), "vornik-companion") {
		t.Fatal("a refusal writes no entry")
	}
	if _, err := os.Stat(filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")); !os.IsNotExist(err) {
		t.Fatal("a refusal writes no key file")
	}
}

func TestCompanionConnect_589_RefusesAWrongKeyAndNoKey(t *testing.T) {
	desktopHome(t)
	srv := fakeCompanion(t, false)
	defer srv.Close()
	companionDeskURL = srv.URL
	if err := runCompanionConnect(context.Background(), "claude-desktop", nil, io.Discard); err == nil || !strings.Contains(err.Error(), "--key-stdin") {
		t.Fatalf("no key: %v", err)
	}
	t.Setenv("VORNIK_COMPANION_TOKEN", "sk-vornik-wrong")
	if err := runCompanionConnect(context.Background(), "claude-desktop", nil, io.Discard); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong key: %v", err)
	}
	if err := runCompanionConnect(context.Background(), "codex", nil, io.Discard); err == nil || !strings.Contains(err.Error(), "plugins") {
		t.Fatalf("another client: %v", err)
	}
}

func TestCompanionDoctor_589(t *testing.T) {
	_, cfgPath := desktopHome(t)
	srv := fakeCompanion(t, false)
	defer srv.Close()
	companionDeskURL = srv.URL
	var out bytes.Buffer
	if n := runCompanionDoctor(context.Background(), &out); n == 0 || !strings.Contains(out.String(), "companion connect claude-desktop") {
		t.Fatalf("not connected: %d failures\n%s", n, out.String())
	}
	// An npx mcp-remote bridge to the companion endpoint is named.
	_ = os.WriteFile(cfgPath, []byte(`{"mcpServers":{"vornik":{"command":"/usr/bin/npx","args":["mcp-remote","`+srv.URL+`/api/v1/mcp/companion"]}}}`), 0o600)
	out.Reset()
	runCompanionDoctor(context.Background(), &out)
	if !strings.Contains(out.String(), "npx needs Node") {
		t.Fatalf("npx entry not named:\n%s", out.String())
	}
	// Healthy after connect; the env token is information, not a failure.
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	if err := runCompanionConnect(context.Background(), "claude-desktop", nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(mustRead(t, cfgPath), &m); err != nil {
		t.Fatal(err)
	}
	delete(m["mcpServers"], "vornik") // the operator removed the npx entry
	b, _ := json.Marshal(m)
	_ = os.WriteFile(cfgPath, b, 0o600)
	out.Reset()
	if n := runCompanionDoctor(context.Background(), &out); n != 0 {
		t.Fatalf("healthy setup reported %d failures:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "the key works (project companion-t") || !strings.Contains(out.String(), "INFO VORNIK_COMPANION_TOKEN is set") {
		t.Fatalf("doctor output:\n%s", out.String())
	}
	if strings.Contains(out.String(), testCompanionKey) {
		t.Fatal("doctor printed the key")
	}
	// A revoked key reads as one.
	keyPath, _ := companionKeyFile("default")
	_ = writePrivate(keyPath, []byte("sk-vornik-revoked\n"))
	out.Reset()
	if n := runCompanionDoctor(context.Background(), &out); n == 0 || !strings.Contains(out.String(), "wrong, revoked or not a companion key") {
		t.Fatalf("a revoked key:\n%s", out.String())
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

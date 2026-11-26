package cli

// GitHub #621, LLD 28 B7-B8: `vornikctl companion connect|doctor|disconnect
// cursor`. A real Cursor cannot run in a test, so these drive the commands
// against a fake companion endpoint in a temp HOME, as the Claude Desktop
// tests do.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeScopedCompanion is fakeCompanion whose whoami carries the key's default
// repo scope (empty scope = an unscoped key).
func fakeScopedCompanion(t *testing.T, scope string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			result = map[string]any{"tools": []map[string]any{{"name": "recall"}, {"name": "whoami"}}}
		case "tools/call":
			who, _ := json.Marshal(map[string]string{"project_id": "companion-t", "effective_repo_scope": scope, "default_repo_scope": scope})
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(who)}}}
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
}

func cursorHome(t *testing.T) (home, pluginDir string) {
	t.Helper()
	home, _ = desktopHome(t)
	companionAllowUnscoped = false
	return home, filepath.Join(home, ".cursor", "plugins", "local", "vornik-companion")
}

func TestCompanionConnectCursor_621_InstallsThePlugin(t *testing.T) {
	home, pluginDir := cursorHome(t)
	srv := fakeScopedCompanion(t, "github.com/o/r")
	defer srv.Close()
	companionDeskURL = srv.URL
	t.Setenv("VORNIK_COMPANION_TOKEN", testCompanionKey)
	var out bytes.Buffer
	if err := runCompanionConnect(context.Background(), "cursor", nil, &out); err != nil {
		t.Fatalf("connect cursor: %v\n%s", err, out.String())
	}
	for _, f := range []string{".cursor-plugin/plugin.json", "rules/vornik.mdc", "skills/delegate/SKILL.md", "mcp.json"} {
		if _, err := os.Stat(filepath.Join(pluginDir, f)); err != nil {
			t.Errorf("plugin file %s not installed: %v", f, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(pluginDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	v := cfg.MCPServers["vornik"]
	if !filepath.IsAbs(v.Command) {
		t.Fatalf("the installed entry must name vornikctl by its absolute path, got %q", v.Command)
	}
	if strings.Join(v.Args, " ") != "companion mcp-bridge --name default --url "+srv.URL {
		t.Fatalf("installed bridge args %v", v.Args)
	}
	// LLD 28 B5: the session hook runs the same binary for the same key and URL.
	hooksRaw, err := os.ReadFile(filepath.Join(pluginDir, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hooks struct {
		Version int `json:"version"`
		Hooks   struct {
			SessionStart []struct {
				Command string `json:"command"`
			} `json:"sessionStart"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(hooksRaw, &hooks); err != nil || hooks.Version != 1 || len(hooks.Hooks.SessionStart) != 1 {
		t.Fatalf("installed hooks.json: %v\n%s", err, hooksRaw)
	}
	wantHook := "'" + v.Command + "' companion session-start --name 'default' --url '" + srv.URL + "'"
	if got := hooks.Hooks.SessionStart[0].Command; got != wantHook {
		t.Fatalf("sessionStart command %q, want %q", got, wantHook)
	}
	keyPath := filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v mode %v", err, fi)
	}
	if strings.Contains(out.String(), testCompanionKey) || !strings.Contains(out.String(), "Reload") {
		t.Fatalf("output must not hold the key and must say to reload Cursor:\n%s", out.String())
	}
	_ = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || p == keyPath {
			return nil
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(testCompanionKey)) {
			t.Errorf("%s holds the key; only the key file may", p)
		}
		return nil
	})

	// doctor reports the install as current.
	var doc bytes.Buffer
	if failed := runCompanionDoctorCursor(context.Background(), &doc); failed != 0 {
		t.Fatalf("doctor after connect: %d failure(s)\n%s", failed, doc.String())
	}

	// Review 737d finding 1: doctor also checks the session hook's binary.
	if err := os.WriteFile(filepath.Join(pluginDir, "hooks", "hooks.json"),
		[]byte(`{"version":1,"hooks":{"sessionStart":[{"command":"'/nonexistent/vornikctl' companion session-start --name 'default'"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	doc.Reset()
	if failed := runCompanionDoctorCursor(context.Background(), &doc); failed == 0 || !strings.Contains(doc.String(), "session hook") {
		t.Fatalf("doctor must fail a hook whose binary is gone:\n%s", doc.String())
	}

	// A second connect replaces the plugin; disconnect removes it and the key.
	if err := runCompanionConnect(context.Background(), "cursor", nil, &out); err != nil {
		t.Fatalf("second connect: %v", err)
	}
	out.Reset()
	if err := runCompanionDisconnect("cursor", &out); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	// Found in the opencode live check, 2026-11-21: the shared tail told the
	// user to remove an entry disconnect had just removed.
	if strings.Contains(out.String(), "yourself") {
		t.Errorf("disconnect removed the plugin but tells the user to remove the entry:\n%s", out.String())
	}
	for _, p := range []string{pluginDir, keyPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survives disconnect", p)
		}
	}
}

func TestCompanionConnectCursor_621_RefusalsWriteNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		key, scope string
		allow      bool
	}{
		"wrong key":         {key: "sk-vornik-companion-x.wrong", scope: "github.com/o/r"},
		"unscoped key":      {key: testCompanionKey, scope: ""},
		"unscoped, allowed": {key: testCompanionKey, scope: "", allow: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, pluginDir := cursorHome(t)
			srv := fakeScopedCompanion(t, tc.scope)
			defer srv.Close()
			companionDeskURL = srv.URL
			companionAllowUnscoped = tc.allow
			t.Setenv("VORNIK_COMPANION_TOKEN", tc.key)
			var out bytes.Buffer
			err := runCompanionConnect(context.Background(), "cursor", nil, &out)
			keyPath := filepath.Join(userConfigDir(t), "vornik", "companion", "default.key")
			if tc.allow {
				if err != nil {
					t.Fatalf("--allow-unscoped must connect: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("connect must refuse")
			}
			if tc.scope == "" && !strings.Contains(err.Error(), "--repo-scope") {
				t.Errorf("an unscoped key's refusal must say how to scope it: %v", err)
			}
			for _, p := range []string{pluginDir, keyPath} {
				if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
					t.Errorf("a refusal wrote %s", p)
				}
			}
		})
	}
}

// disconnect removes only a directory that is this plugin.
func TestCompanionDisconnectCursor_621_LeavesAForeignDirectory(t *testing.T) {
	_, pluginDir := cursorHome(t)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCompanionDisconnect("cursor", &out); err == nil {
		t.Fatal("disconnect must refuse to remove a directory that is not the Vornik plugin")
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "notes.txt")); err != nil {
		t.Fatal("a foreign directory was touched")
	}
}

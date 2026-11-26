//go:build integration

package api

// GitHub #621, LLD 28 B10 (integration): the Cursor plugin as Cursor runs it.
// A built vornikctl connects to the real companion MCP handler, then the test
// runs the installed plugin's MCP entry (the stdio bridge) and its
// sessionStart hook command exactly as written into ~/.cursor, and drives
// initialize, tools/list and whoami through the bridge. No Cursor is
// involved: the live Cursor session is the operator's step (LLD 28 B10).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

func buildVornikctlForCursor(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	bin := filepath.Join(t.TempDir(), "vornikctl")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/vornikctl")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "build vornikctl: %s", out)
	return bin
}

func TestCursorPlugin_621_BridgeAndHookAgainstTheCompanionHandler(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, err := apikey.Generate("alpha")
	require.NoError(t, err)
	require.NoError(t, keyRepo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-co-cursor", ProjectID: "alpha", Name: "cursor-1",
		KeyHash: apikey.Hash(raw), KeyPrefix: apikey.DisplayPrefix(raw),
		ClientKind: "cursor", DefaultRepoScope: "github.com/acme/widgets",
		CreatedAt: time.Now().UTC(),
	}))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mcp/companion" {
			http.NotFound(w, r)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		srv.CompanionMCPHandler(w, withCompanionBearer(r, bearer))
	}))
	defer ts.Close()

	bin := buildVornikctlForCursor(t)
	home := t.TempDir()
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=" + os.Getenv("PATH")}

	connect := exec.Command(bin, "companion", "connect", "cursor", "--key-stdin", "--url", ts.URL)
	connect.Env = env
	connect.Stdin = strings.NewReader(raw + "\n")
	out, err := connect.CombinedOutput()
	require.NoErrorf(t, err, "connect cursor: %s", out)
	require.NotContains(t, string(out), raw, "connect must never print the key")

	pluginDir := filepath.Join(home, ".cursor", "plugins", "local", "vornik-companion")
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	mcpRaw, err := os.ReadFile(filepath.Join(pluginDir, "mcp.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(mcpRaw, &mcp))
	entry := mcp.MCPServers["vornik"]
	require.Equal(t, bin, entry.Command)

	// The bridge, as Cursor starts it: newline-delimited JSON-RPC on stdio.
	bridge := exec.Command(entry.Command, entry.Args...)
	bridge.Env = env
	var in bytes.Buffer
	for i, req := range []map[string]any{
		{"method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "cursor-test", "version": "1"}}},
		{"method": "tools/list", "params": map[string]any{}},
		{"method": "tools/call", "params": map[string]any{"name": "whoami", "arguments": map[string]any{}}},
	} {
		req["jsonrpc"], req["id"] = "2.0", i+1
		line, _ := json.Marshal(req)
		in.Write(append(line, '\n'))
	}
	bridge.Stdin = &in
	bridgeOut, err := bridge.Output()
	require.NoErrorf(t, err, "bridge: %s", bridgeOut)
	var replies []string
	sc := bufio.NewScanner(bytes.NewReader(bridgeOut))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			replies = append(replies, sc.Text())
		}
	}
	require.Lenf(t, replies, 3, "three replies through the bridge, got:\n%s", bridgeOut)
	// The test server wires no memory store, so the memory tools are not
	// offered; delegate and whoami are.
	require.Contains(t, replies[1], `"name":"delegate"`, "tools/list through the bridge names the companion tools")
	require.Contains(t, replies[1], `"name":"whoami"`)
	require.Contains(t, replies[2], "github.com/acme/widgets", "whoami through the bridge reports the key's scope")

	// The sessionStart hook, as Cursor runs it: through a shell.
	hooksRaw, err := os.ReadFile(filepath.Join(pluginDir, "hooks", "hooks.json"))
	require.NoError(t, err)
	var hooks struct {
		Hooks struct {
			SessionStart []struct {
				Command string `json:"command"`
			} `json:"sessionStart"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(hooksRaw, &hooks))
	require.Len(t, hooks.Hooks.SessionStart, 1)
	hook := exec.Command("sh", "-c", hooks.Hooks.SessionStart[0].Command)
	hook.Env = env
	hookOut, err := hook.Output()
	require.NoErrorf(t, err, "session hook: %s", hookOut)
	var digest struct {
		Context string `json:"additional_context"`
	}
	require.NoErrorf(t, json.Unmarshal(hookOut, &digest), "hook output: %s", hookOut)
	require.Contains(t, digest.Context, "Vornik session digest")
	require.NotContains(t, digest.Context, "unavailable (no companion key", "the hook found the key file connect wrote")
	require.NotContains(t, string(hookOut), raw)
}

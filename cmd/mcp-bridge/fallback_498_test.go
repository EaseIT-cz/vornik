package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #498 (2026-10-21): with no VORNIK_API_URL the bridge read mcp.json
// and ran its own MCP manager inside the agent container, so a role's call
// reached the server with no approval rows, no #413 live-descriptor gate and
// no #481 sticky suspension: a tool the daemon holds as suspended ran. The
// local mode is removed. Without a daemon URL the bridge refuses every verb
// and starts nothing, whatever $MCP_CONFIG names.
//
// Found at: review (lane W18-C). Tested at run(), the binary's whole entry,
// which the agent entrypoint invokes for every MCP discover and call.
func TestBridge498_NoDaemonURLRefusesAndStartsNothing(t *testing.T) {
	for _, verb := range [][]string{
		{"discover"},
		{"call", "mcp__files__delete_file", `{"path":"x"}`},
	} {
		t.Run(verb[0], func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "server-started")
			cfg := filepath.Join(dir, "mcp.json")
			// A stdio "server" whose only effect is proving it was started.
			body := `[{"name":"files","transport":"stdio","command":"/bin/sh","args":["-c","touch ` + marker + `"]}]`
			if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(envAPIURL, "")
			t.Setenv(envProjectID, "p498")
			t.Setenv("MCP_CONFIG", cfg)
			oldArgs := os.Args
			os.Args = append([]string{"mcp-bridge"}, verb...)
			defer func() { os.Args = oldArgs }()

			out, err := captureStdout(t, run)
			if err == nil || !strings.Contains(err.Error(), envAPIURL) {
				t.Fatalf("run() error = %v (stdout %q); want a refusal naming %s", err, out, envAPIURL)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatal("the bridge started the mcp.json server: the local MCP fallback still exists")
			}
		})
	}
}

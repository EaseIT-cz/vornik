package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #86 / LLD 21 §D2 "The generic harness contract" (2026-10-08, D6): a
// harness nobody has written an adapter for connects without a daemon change
// and without connect editing a file it does not know. `vornikctl agent
// connect generic --print` mints the namespace's key as connect does, stores
// it in the 0600 key file, writes no harness file, and prints the MCP entry in
// both shapes for the person to paste.
func TestAgentConnectGeneric_PrintsEntryAndWritesNoFile(t *testing.T) {
	d := defaultDaemon()
	c, user := newTestConnector(t, "generic", d, keyAbsent)
	c.print = true
	c.clientKind = "goose"
	c.accept = true // the generic harness is shell-capable; the gate is exercised elsewhere
	var out, info bytes.Buffer
	c.info = &info
	if err := c.connect(&out); err != nil {
		t.Fatalf("connect generic: %v\nout:\n%s\ninfo:\n%s", err, out.String(), info.String())
	}

	var printed struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
		StreamableHTTP struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			KeyFile string            `json:"key_file"`
		} `json:"streamable_http"`
	}
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out.String())
	}
	entry, ok := printed.MCPServers["vornik-generic"]
	if !ok || entry.Command != c.bridge || !strings.Contains(strings.Join(entry.Args, " "), "mcp-bridge --namespace generic") {
		t.Fatalf("stdio entry: %+v", printed.MCPServers)
	}
	if printed.StreamableHTTP.URL != c.client.baseURL+"/api/v1/mcp/companion" {
		t.Fatalf("streamable HTTP url = %q", printed.StreamableHTTP.URL)
	}
	if strings.Contains(out.String(), connectSecret) || strings.Contains(info.String(), connectSecret) {
		t.Fatal("the key was printed")
	}
	if g := d.grants[0]; g["agentAdmin"] != true || g["namespace"] != "generic" || g["clientKind"] != "goose" {
		t.Fatalf("grant body: %+v", g)
	}

	keyPath := filepath.Join(user.configDir, "vornik", "agents", "generic.key")
	if printed.StreamableHTTP.KeyFile != keyPath {
		t.Fatalf("key_file = %q, want %q", printed.StreamableHTTP.KeyFile, keyPath)
	}
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	// Nothing under HOME but the key file and its record.
	var files []string
	_ = filepath.Walk(user.home, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 2 || !containsPath(files, keyPath) || !containsPath(files, strings.TrimSuffix(keyPath, ".key")+".json") {
		t.Fatalf("connect generic wrote %v; want only the key file and its record", files)
	}
}

// --print is the generic harness's way of connecting, and only its.
func TestAgentConnect_PrintOnlyForGeneric(t *testing.T) {
	d := defaultDaemon()
	c, _ := newTestConnector(t, "hermes", d, keyAbsent)
	c.print = true
	if err := c.connect(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--print") {
		t.Fatalf("connect hermes --print = %v; want a refusal naming --print", err)
	}
	g, _ := newTestConnector(t, "generic", d, keyAbsent)
	if err := g.connect(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--print") {
		t.Fatalf("connect generic without --print = %v; want a refusal naming --print", err)
	}
	h, _ := newTestConnector(t, "hermes", d, keyAbsent)
	h.clientKind = "goose"
	if err := h.connect(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--client") {
		t.Fatalf("connect hermes --client = %v; want a refusal naming --client", err)
	}
	if len(d.grants) != 0 {
		t.Fatalf("a refused connect minted a key: %v", d.grants)
	}
}

func containsPath(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

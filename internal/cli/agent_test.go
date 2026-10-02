package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plan P6.1: the bridge command reads <config>/vornik/agents/<ns>.key and
// relays to <url>/api/v1/mcp/companion with it; the key never reaches
// stdout or stderr. Control: runMCPBridge.
func TestRunMCPBridge(t *testing.T) {
	const key = "sk-vornik-hermes-CANARY77"
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(srv.Close)
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("VORNIK_API_URL", "")
	agentNamespace, agentURL = "hermes", srv.URL
	t.Cleanup(func() { agentNamespace, agentURL = "", "" })

	var out, errw bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	if err := runMCPBridge(context.Background(), in, &out, &errw); err == nil {
		t.Fatal("ran with no key file")
	}

	dir := filepath.Join(cfg, "vornik", "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hermes.key"), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	if err := runMCPBridge(context.Background(), in, &out, &errw); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+key || gotPath != "/api/v1/mcp/companion" {
		t.Fatalf("sent %q to %s", gotAuth, gotPath)
	}
	if !strings.Contains(out.String(), `"result"`) || strings.Contains(out.String()+errw.String(), key) {
		t.Fatalf("stdout %q stderr %q", out.String(), errw.String())
	}

	// Plain http to another machine is refused before the key is read.
	agentURL = "http://192.0.2.1:8080"
	if err := runMCPBridge(context.Background(), strings.NewReader(""), &out, &errw); err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("cleartext endpoint: %v", err)
	}
}

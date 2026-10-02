package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

const connectSecret = "sk-vornik-hermes--home-CANARYSECRET42"

// connectDaemon serves what connect and disconnect call.
type connectDaemon struct {
	mu        sync.Mutex
	features  map[string]bool
	host      map[string]any
	grantCode string // an error code to answer the grant with
	grants    []map[string]any
	revoked   []string
	revokeKey []string // the X-API-Key each revoke carried
}

func (d *connectDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/capabilities":
			body := map[string]any{"features": d.features}
			if d.host != nil {
				body["host"] = d.host
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/companion/grant":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			d.grants = append(d.grants, b)
			if d.grantCode != "" {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"code":"` + d.grantCode + `","message":"no"}}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "akey-1", "projectId": "hermes--home", "clientKind": b["clientKind"], "secret": connectSecret})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
			d.revoked = append(d.revoked, r.URL.Path)
			d.revokeKey = append(d.revokeKey, r.Header.Get("X-API-Key"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestConnector(t *testing.T, harness string, d *connectDaemon, access keyAccess) (*connector, agentUser) {
	t.Helper()
	home := t.TempDir()
	user := agentUser{home: home, configDir: filepath.Join(home, ".config"), getenv: func(string) string { return "" }, uid: os.Getuid()}
	srv := d.server(t)
	c := newConnectorFor(harness, "", "/usr/bin/vornikctl")
	c.client, c.user, c.probe = NewClient(srv.URL, "sk-operator"), user, func(string) keyAccess { return access }
	return c, user
}

func defaultDaemon() *connectDaemon {
	return &connectDaemon{features: map[string]bool{"companion-admin": true},
		host: map[string]any{"daemon_uid": 990, "daemon_containerized": false, "store_key_path": "/var/lib/vornik/secrets/store.key"}}
}

// Plan P6.2: connect mints with the operator key (agentAdmin, the
// namespace, the harness as client kind), stores the key 0600 in a 0700
// directory, and writes an entry that holds no key; disconnect revokes,
// removes the entry (another entry survives) and the key file (review
// 6f6b F10). Control: connect and disconnectAgent.
func TestAgentConnect_RoundTrip(t *testing.T) {
	d := defaultDaemon()
	c, user := newTestConnector(t, "hermes", d, keyAbsent)
	cfg := filepath.Join(user.home, ".hermes", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("# mine\nmcp_servers:\n  files:\n    command: fs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := c.connect(&out); err != nil {
		t.Fatalf("connect: %v\n%s", err, out.String())
	}
	if g := d.grants[0]; g["agentAdmin"] != true || g["namespace"] != "hermes" || g["clientKind"] != "hermes" {
		t.Fatalf("grant body: %+v", g)
	}
	keyPath := filepath.Join(user.configDir, "vornik", "agents", "hermes.key")
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Dir(keyPath)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("key dir: %v", fi.Mode())
	}
	b, _ := os.ReadFile(cfg)
	// The entry carries the daemon's URL: the harness starts the bridge with
	// its own environment (DoD lane bring-up, 2026-10-02).
	if !strings.Contains(string(b), "--url") || !strings.Contains(string(b), c.client.baseURL) {
		t.Fatalf("the entry has no --url:\n%s", b)
	}
	if !strings.Contains(string(b), "vornik-hermes") || !strings.Contains(string(b), "# mine") || strings.Contains(string(b), connectSecret) {
		t.Fatalf("harness config:\n%s", b)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o644 {
		t.Fatalf("harness config mode changed: %v", fi.Mode())
	}
	if strings.Contains(out.String(), connectSecret) {
		t.Fatal("the key was printed")
	}
	for _, dir := range []string{user.home} {
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() && p != keyPath {
				if raw, _ := os.ReadFile(p); strings.Contains(string(raw), connectSecret) {
					t.Errorf("the key is in %s", p)
				}
			}
			return nil
		})
	}

	out.Reset()
	if err := disconnectAgent(c.client, user, "hermes", &out); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if len(d.revoked) != 1 || d.revoked[0] != "/api/v1/projects/hermes--home/keys/akey-1" {
		t.Fatalf("revoked: %v", d.revoked)
	}
	// Review 20261002-00bc F2: the revoke uses the operator key, never
	// the agent's own.
	if d.revokeKey[0] != "sk-operator" {
		t.Fatalf("revoked with %q", d.revokeKey[0])
	}
	b, _ = os.ReadFile(cfg)
	if strings.Contains(string(b), "vornik-hermes") || !strings.Contains(string(b), "files:") {
		t.Fatalf("after disconnect:\n%s", b)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("the key file is still there")
	}
}

// Review 6f6b F1: the gate's control is readability of the store key, for
// a shell-capable harness only; a shared UID alone neither refuses nor
// allows. Control: sharedUserGate, through connect.
func TestAgentConnect_SharedUserGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		access  keyAccess
		accept  bool
		ok      bool
		says    string
	}{
		{"readable", "codex", keyReadable, false, false, "is readable to you"},
		{"readable, accepted", "codex", keyReadable, true, true, "Connecting anyway"},
		{"denied, same UID", "claude-code", keyDenied, false, true, "not readable to you"},
		{"absent", "codex", keyAbsent, false, false, "could not be checked"},
		{"absent, accepted", "codex", keyAbsent, true, true, "could not be checked"},
		{"MCP-only, readable", "hermes", keyReadable, false, true, "only through its MCP connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := defaultDaemon()
			d.host["daemon_uid"] = os.Getuid() // a shared UID never decides
			c, _ := newTestConnector(t, tc.harness, d, tc.access)
			c.accept, c.dry = tc.accept, true
			var out bytes.Buffer
			err := c.connect(&out)
			if (err == nil) != tc.ok || !strings.Contains(out.String(), tc.says) {
				t.Fatalf("err %v\n%s", err, out.String())
			}
			if len(d.grants) != 0 {
				t.Fatal("a dry run or a refusal minted a key")
			}
		})
	}
	// Plan P6 amendment F8: the gate uses the shared predicate, so an
	// unknown kind is shell-capable there too.
	if ok, _ := sharedUserGate(agentadmin.HarnessClassOf("something-new"), keyReadable, false); ok {
		t.Fatal("an unknown harness kind passed the gate")
	}
	// A containerised daemon's paths are not the host's: never probed.
	d := defaultDaemon()
	d.host["daemon_containerized"] = true
	c, _ := newTestConnector(t, "codex", d, keyDenied)
	c.dry = true
	if err := c.connect(&bytes.Buffer{}); err == nil {
		t.Fatal("a containerised daemon passed on a host probe")
	}
}

// Connect's refusals map to what the person should do, and mint nothing
// they would have to clean up.
func TestAgentConnect_Refusals(t *testing.T) {
	d := defaultDaemon()
	d.grantCode = "NO_APPROVER_DEVICE"
	c, user := newTestConnector(t, "hermes", d, keyAbsent)
	if err := c.connect(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "vornikctl pair-device") {
		t.Fatalf("no device: %v", err)
	}
	if _, err := os.Stat(filepath.Join(user.configDir, "vornik", "agents", "hermes.key")); !os.IsNotExist(err) {
		t.Fatal("a key file was written after a refused grant")
	}
	d.grantCode = "NAMESPACE_TAKEN"
	if err := c.connect(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "vornikctl agent disconnect hermes") {
		t.Fatalf("taken: %v", err)
	}
	d.grantCode = ""
	d.features["companion-admin"] = false
	if err := c.connect(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "agent_admin.enabled") || !strings.Contains(err.Error(), "install-config-assets") {
		t.Fatalf("disabled: %v", err)
	}
	// A harness file the edit cannot handle is refused before minting.
	d.features["companion-admin"] = true
	c2, user2 := newTestConnector(t, "codex", d, keyDenied)
	cfg := filepath.Join(user2.home, ".codex", "config.toml")
	_ = os.MkdirAll(filepath.Dir(cfg), 0o700)
	_ = os.WriteFile(cfg, []byte("[[mcp_servers.vornik-codex]]\n"), 0o600) // codex's default namespace is codex
	before := len(d.grants)
	if err := c2.connect(&bytes.Buffer{}); err == nil || len(d.grants) != before {
		t.Fatalf("unhandled TOML: %v, grants %d", err, len(d.grants)-before)
	}
	// A JSON harness file that does not parse is refused before minting
	// and left as it was (review 20261002-00bc F3).
	c4, user4 := newTestConnector(t, "claude-desktop", d, keyAbsent)
	desk := filepath.Join(user4.configDir, "Claude", "claude_desktop_config.json")
	_ = os.MkdirAll(filepath.Dir(desk), 0o700)
	_ = os.WriteFile(desk, []byte(`{"mcpServers": {"x": 1},}`), 0o600)
	before = len(d.grants)
	if err := c4.connect(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), desk) || len(d.grants) != before {
		t.Fatalf("malformed JSON: %v, grants %d", err, len(d.grants)-before)
	}
	if b, _ := os.ReadFile(desk); string(b) != `{"mcpServers": {"x": 1},}` {
		t.Fatalf("the malformed file was changed: %s", b)
	}
	// Plain http to another machine is refused before anything is sent.
	c3, _ := newTestConnector(t, "hermes", d, keyAbsent)
	c3.client.baseURL = "http://192.0.2.7:8080"
	if err := c3.connect(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("cleartext: %v", err)
	}
}

// A failure after the mint revokes the key and leaves no key file.
func TestAgentConnect_UndoesAfterMint(t *testing.T) {
	d := defaultDaemon()
	c, user := newTestConnector(t, "hermes", d, keyAbsent)
	// The key directory is a symlink: writing the key fails after the mint.
	agents := filepath.Join(user.configDir, "vornik", "agents")
	_ = os.MkdirAll(filepath.Dir(agents), 0o700)
	if err := os.Symlink(t.TempDir(), agents); err != nil {
		t.Fatal(err)
	}
	if err := c.connect(&bytes.Buffer{}); err == nil {
		t.Fatal("connected through a symlinked key directory")
	}
	if len(d.revoked) != 1 {
		t.Fatalf("the minted key was not revoked: %v", d.revoked)
	}
}

// A namespace is letters and digits only, so the default drops the
// harness name's dashes. Control: newConnectorFor.
func TestAgentConnect_DefaultNamespace(t *testing.T) {
	for h, want := range map[string]string{"hermes": "hermes", "claude-code": "claudecode", "claude-desktop": "claudedesktop", "codex": "codex"} {
		if got := newConnectorFor(h, "", "/x").ns; got != want {
			t.Errorf("%s: %s, want %s", h, got, want)
		}
	}
	if got := newConnectorFor("codex", "work", "/x").ns; got != "work" {
		t.Errorf("--namespace ignored: %s", got)
	}
}

// Review 20261002-d930 F3: disconnect removes the entry where the harness
// reads it now. A recorded path that no longer exists falls back to the
// re-derived one, and nothing is created at either. Control: the path
// choice in disconnectAgent.
func TestAgentDisconnect_StaleRecordedPath(t *testing.T) {
	d := defaultDaemon()
	c, user := newTestConnector(t, "hermes", d, keyAbsent)
	if err := c.connect(&bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(user.home, ".hermes", "config.yaml")
	moved := filepath.Join(user.home, "hermes-new", "config.yaml")
	_ = os.MkdirAll(filepath.Dir(moved), 0o700)
	if err := os.Rename(cfg, moved); err != nil {
		t.Fatal(err)
	}
	user.getenv = func(k string) string {
		if k == "HERMES_HOME" {
			return filepath.Dir(moved)
		}
		return ""
	}
	if err := disconnectAgent(c.client, user, "hermes", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(moved); strings.Contains(string(b), "vornik-hermes") {
		t.Fatalf("the entry is still in the harness's current file:\n%s", b)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("disconnect created a file at the stale recorded path")
	}
}

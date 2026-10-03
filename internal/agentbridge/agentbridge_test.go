package agentbridge

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
	"sync"
	"testing"
	"time"
)

const testKey = "sk-vornik-hermes-CANARYKEY0123"

type fakeCompanion struct {
	mu       sync.Mutex
	sessions []string
	methods  []string
	versions []string // MCP-Protocol-Version per request
}

func (f *fakeCompanion) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.sessions = append(f.sessions, r.Header.Get("Mcp-Session-Id"))
		f.methods = append(f.methods, req.Method)
		f.versions = append(f.versions, r.Header.Get("MCP-Protocol-Version"))
		f.mu.Unlock()
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`))
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"tools":[{"name":"list_my_setup"}]}}`))
		case "sse":
			w.Header().Set("Content-Type", "text/event-stream")
			// A server notification before the response (review 6f6b F2):
			// both are legal stdio messages, written in order.
			_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
				"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":" + string(req.ID) + ",\"result\":{}}\n\ndata: not json\n\n"))
		default:
			_, _ = w.Write([]byte("{\n  \"jsonrpc\": \"2.0\",\n  \"id\": " + string(req.ID) + ",\n  \"result\": {\"ok\": true}\n}"))
		}
	})
}

func runBridge(t *testing.T, endpoint, key, input string) (string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(input), &out, &errw, Config{Endpoint: endpoint, Key: key}); err != nil {
		t.Fatalf("run: %v", err)
	}
	return out.String(), errw.String()
}

// Agent-administered Vornik plan P6.1: the bridge relays newline-delimited
// JSON-RPC to the companion endpoint, one compact line per response, none
// for a notification; it keeps the MCP session ID; an HTTP error becomes a
// JSON-RPC error; the key never appears on stdout or stderr. Control: Run.
func TestBridge_Relays(t *testing.T) {
	f := &fakeCompanion{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":"x","method":"sse"}
`
	out, errw := runBridge(t, srv.URL, testKey, in)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("stdout lines = %d: %q", len(lines), out)
	}
	for _, l := range lines {
		if !json.Valid([]byte(l)) || strings.Contains(l, "\n") {
			t.Fatalf("not one compact JSON line: %q", l)
		}
	}
	if !strings.Contains(lines[1], "list_my_setup") || !strings.Contains(lines[2], "notifications/progress") || !strings.Contains(lines[3], `"id":"x"`) {
		t.Fatalf("responses: %q", lines)
	}
	// The notification was forwarded (review 7db7 F4) and its 202 gave
	// no stdout line and no error.
	if len(f.methods) != 4 || f.methods[1] != "notifications/initialized" {
		t.Fatalf("methods sent: %q", f.methods)
	}
	if f.sessions[0] != "" || f.sessions[2] != "sess-1" {
		t.Fatalf("session IDs sent: %q", f.sessions)
	}
	// Review 6f6b F5: the negotiated protocol version rides every later request.
	if f.versions[0] != "" || f.versions[1] != "2025-06-18" || f.versions[3] != "2025-06-18" {
		t.Fatalf("MCP-Protocol-Version sent: %q", f.versions)
	}
	if strings.Contains(out+errw, testKey) {
		t.Fatal("the key reached stdout or stderr")
	}
	if !strings.Contains(errw, "dropped a reply that is not JSON") || strings.Contains(errw, "not json") {
		t.Fatalf("the non-JSON SSE event: stderr %q", errw)
	}

	// A wrong key: the server's 401 becomes a JSON-RPC error with the id.
	out, errw = runBridge(t, srv.URL, "sk-wrong", `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`+"\n")
	var resp struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil || resp.ID != 7 || resp.Error.Code != -32000 || !strings.Contains(resp.Error.Message, "401") {
		t.Fatalf("401 relay: %q %v", out, err)
	}
	if strings.Contains(out+errw, "sk-wrong") {
		t.Fatal("the key reached stdout or stderr")
	}
}

// The key file must be the user's own and not readable by anyone else.
func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hermes.key")
	if err := os.WriteFile(p, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := LoadKey(p); err != nil || k != testKey {
		t.Fatalf("load: %q %v", k, err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(p); err == nil {
		t.Fatal("a world-readable key file was accepted")
	}
	if _, err := LoadKey(filepath.Join(dir, "absent.key")); err == nil {
		t.Fatal("a missing key file was accepted")
	}
	// Review 6f6b F11: a symlink is refused, even to a 0600 file of the
	// user's own, so a swap cannot redirect the bridge.
	target := filepath.Join(dir, "elsewhere.key")
	if err := os.WriteFile(target, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked key file: %v", err)
	}
}

// Plan P6 global rules: the key file is <config>/vornik/agents/<ns>.key, and
// a namespace that is not one cannot name another path. Control: KeyPath.
func TestKeyPath(t *testing.T) {
	if p, err := KeyPath("/home/u/.config", "hermes"); err != nil || p != "/home/u/.config/vornik/agents/hermes.key" {
		t.Fatalf("KeyPath: %q %v", p, err)
	}
	for _, ns := range []string{"", "../x", "a/b", "Hermes", "a--b"} {
		if p, err := KeyPath("/home/u/.config", ns); err == nil {
			t.Errorf("namespace %q gave %s", ns, p)
		}
	}
}

// The bridge sends the key as a bearer token, so it refuses to send it in
// cleartext anywhere but this machine. Control: CheckEndpoint.
func TestCheckEndpoint(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://vornik.example": true, "http://127.0.0.1:8080": true, "http://localhost:8080": true, "http://[::1]:8080": true,
		"http://192.168.0.142:8080": false, "http://vornik.example": false, "ftp://x": false, "not a url": false,
	} {
		if err := CheckEndpoint(u); (err == nil) != ok {
			t.Errorf("CheckEndpoint(%q) = %v, want ok=%v", u, err, ok)
		}
	}
}

// Plan P6.5: a revoked key's 401 tells the person what to do, not just the
// status. Control: the 401 branch of post.
func TestBridge_UnauthorizedSaysReconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(srv.Close)
	out, _ := runBridge(t, srv.URL, testKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	if !strings.Contains(out, "vornikctl agent connect") || !strings.Contains(out, "401") {
		t.Fatalf("401: %q", out)
	}
}

// Review 20261002-3b9f F1: an SSE reply is relayed event by event as it
// arrives, not when the stream closes, so a progress notification reaches
// the harness while the call is still running. Control: the streaming read
// in post.
func TestBridge_StreamsSSE(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`+"\n"), pw, io.Discard, Config{Endpoint: srv.URL, Key: testKey})
		_ = pw.Close()
	}()
	lines := make(chan string, 2)
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := pr.Read(buf)
			acc += string(buf[:n])
			for strings.Contains(acc, "\n") {
				i := strings.Index(acc, "\n")
				lines <- acc[:i]
				acc = acc[i+1:]
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	select {
	case l := <-lines:
		if !strings.Contains(l, "notifications/progress") {
			t.Fatalf("first line: %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notification was held until the stream closed")
	}
	close(release)
	if l := <-lines; !strings.Contains(l, `"id":1`) {
		t.Fatalf("second line: %q", l)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Review 20261002-3b9f F2: a key file owned by another user is refused.
// Control: the owner check in LoadKey, through the currentUID seam.
func TestLoadKey_OtherOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hermes.key")
	if err := os.WriteFile(p, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	origUID := currentUID
	t.Cleanup(func() { currentUID = origUID })
	currentUID = func() int { return origUID() + 1 }
	if _, err := LoadKey(p); err == nil || !strings.Contains(err.Error(), "belongs to another user") {
		t.Fatalf("another user's key file: %v", err)
	}
}

// Review 20261003-ff65 item 7 (Hermes approval transport design §4.3: the
// key never reaches stdout, stderr or the plugin): every LoadKey refusal
// names the path and the fix, never the key material. vornikctl agent
// host-approval and mcp-bridge print these errors. Each refusal is reached
// with a file that holds the key.
func TestLoadKey_ErrorsNeverCarryTheKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(testKey+"\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]func() string{
		"world readable": func() string { return write("open.key", 0o644) },
		"symlink": func() string {
			link := filepath.Join(dir, "link.key")
			if err := os.Symlink(write("target.key", 0o600), link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"other owner": func() string { return write("owned.key", 0o600) },
		"missing":     func() string { return filepath.Join(dir, "absent.key") },
	}
	origUID := currentUID
	t.Cleanup(func() { currentUID = origUID })
	examined := 0
	for name, setup := range cases {
		currentUID = origUID
		if name == "other owner" {
			currentUID = func() int { return origUID() + 1 }
		}
		_, err := LoadKey(setup())
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "CANARYKEY") {
			t.Errorf("%s: the error carries the key: %v", name, err)
		}
		examined++
	}
	if examined != len(cases) {
		t.Fatalf("examined %d of %d refusals", examined, len(cases))
	}
}

// Regression (DoD lane bring-up, 2026-10-02): a key the harness's user
// cannot reach (its directory belongs to someone else) was reported as "no
// key at ...; run connect first", which sends the person to reconnect
// instead of to the ownership. Control: LoadKey's permission branch.
func TestLoadKey_PermissionIsNotMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := filepath.Join(t.TempDir(), "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "hermes.key")
	if err := os.WriteFile(p, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := LoadKey(p)
	if err == nil || strings.Contains(err.Error(), "no key at") || !strings.Contains(err.Error(), "the user the harness runs as") {
		t.Fatalf("an unreadable key: %v", err)
	}
}

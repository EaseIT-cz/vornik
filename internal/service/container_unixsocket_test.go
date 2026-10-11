package service

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
)

// GitHub #95 (2026-10-07, T15 listener inventory): the daemon's unix socket
// is mode 0666 and served the SAME http.Server as the TCP port, so any local
// user or daemon-only container reached the UI, the approver pages and the
// admin API through filesystem permission alone. The socket now has its own
// server whose handler serves only the 13 agent routes and answers 404 to
// everything else. Design: cloudflare-real-ip-design.md §12.

// socketProbeMux stands in for the root mux: every path answers 200 with a
// marker, so a 404 can only come from the socket's allowlist filter.
func socketProbeMux() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Probe", "reached")
		w.WriteHeader(http.StatusOK)
	})
}

// startSocket serves the container's agent socket the way Run does, over a
// real unix socket in a temp dir, and returns an HTTP client dialling it.
func startSocket(t *testing.T) (*http.Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "vsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "v.sock")
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Server.UnixSocket = sock
	c.HTTPServer = &http.Server{Handler: socketProbeMux(), ReadHeaderTimeout: time.Second}
	mw, err := c.realIPMiddleware()
	if err != nil {
		t.Fatal(err)
	}
	c.AgentSocketServer = newAgentSocketServer(socketProbeMux(), mw, time.Second, time.Second)
	errCh := make(chan error, 1)
	if err := c.serveAgentUnixSocket(errCh); err != nil {
		t.Fatalf("serveAgentUnixSocket: %v", err)
	}
	t.Cleanup(func() { _ = c.AgentSocketServer.Close() })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	return client, sock
}

func socketStatus(t *testing.T, client *http.Client, method, path string) int {
	t.Helper()
	req, err := http.NewRequest(method, "http://localhost"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Opaque keeps the request line exactly as given (no client-side cleaning).
	req.URL.Opaque = path
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK && resp.Header.Get("X-Probe") != "reached" {
		t.Fatalf("%s %s: 200 without reaching the probe", method, path)
	}
	return resp.StatusCode
}

func TestUnixSocket_UIRouteRefused(t *testing.T) {
	client, _ := startSocket(t)
	for _, p := range []string{"/ui/", "/ui", "/ui/login", "/ui/static/htmx.js"} {
		if got := socketStatus(t, client, http.MethodGet, p); got != http.StatusNotFound {
			t.Errorf("GET %s over the socket = %d, want 404", p, got)
		}
	}
}

func TestUnixSocket_ApproverRouteRefused(t *testing.T) {
	client, _ := startSocket(t)
	for _, p := range []string{"/ui/pair", "/ui/pair/wait", "/ui/approve/abc"} {
		if got := socketStatus(t, client, http.MethodGet, p); got != http.StatusNotFound {
			t.Errorf("GET %s over the socket = %d, want 404", p, got)
		}
	}
}

func TestUnixSocket_AdminAPIRouteRefused(t *testing.T) {
	client, _ := startSocket(t)
	for _, p := range []string{"/api/v1/doctor", "/api/v1/admin/audit", "/api/v1/projects", "/api/v1/projects/p/tasks", "/api/v1/mcp/companion", "/auth/github/start", "/metrics", "/"} {
		if got := socketStatus(t, client, http.MethodGet, p); got != http.StatusNotFound {
			t.Errorf("GET %s over the socket = %d, want 404", p, got)
		}
	}
}

func TestUnixSocket_HealthProbeRefused(t *testing.T) {
	client, _ := startSocket(t)
	for _, p := range []string{"/livez", "/healthz", "/readyz"} {
		if got := socketStatus(t, client, http.MethodGet, p); got != http.StatusNotFound {
			t.Errorf("GET %s over the socket = %d, want 404", p, got)
		}
	}
}

func TestUnixSocket_AgentRoutesServed(t *testing.T) {
	client, _ := startSocket(t)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/projects/p1/tasks/task_1/messages?limit=20"},
		{http.MethodPost, "/api/v1/projects/p1/tasks/task_1/summarize"},
		{http.MethodGet, "/api/v1/projects/p1/skills/fetch?name=a&role=b"},
		{http.MethodGet, "/api/v1/projects/p1/memory/search?limit=5"},
		{http.MethodPost, "/api/v1/projects/p1/api/query"},
		{http.MethodGet, "/api/v1/projects/p1/api/providers"},
		{http.MethodGet, "/api/v1/projects/p1/mcp/tools"},
		{http.MethodPost, "/api/v1/projects/p1/mcp/tools/call"},
		{http.MethodPost, "/api/v1/internal/tool-audit"},
		{http.MethodPost, "/api/v1/internal/llm-usage"},
		{http.MethodPost, "/api/v1/internal/backlog-deposit"},
		{http.MethodPost, "/api/v1/chat/completions"},
		{http.MethodPost, "/v1/chat/completions"},
	}
	if len(routes) != 13 {
		t.Fatalf("the design lists 13 agent routes, the table has %d", len(routes))
	}
	for _, r := range routes {
		if got := socketStatus(t, client, r.method, r.path); got != http.StatusOK {
			t.Errorf("%s %s over the socket = %d, want it to reach the handler", r.method, r.path, got)
		}
	}
}

func TestUnixSocket_UncleanOrDeeperPathRefused(t *testing.T) {
	client, _ := startSocket(t)
	for _, p := range []string{
		"/api/v1/internal/tool-audit/../../doctor",
		"/api/v1/projects/p1/tasks/t/../../../../doctor",
		"/api/v1//internal/tool-audit",
		"/api/v1/internal/llm-usage/",
		"/api/v1/projects/p1/tasks/t/x/messages",
		"/api/v1/projects/p1/memory/search/extra",
		"/api/v1/projects//memory/search",
		"/api/v1/projects/p1%2Fx/memory/search",
		"/api/v1/projects/p1/./memory/search",
	} {
		if got := socketStatus(t, client, http.MethodGet, p); got != http.StatusNotFound {
			t.Errorf("GET %s over the socket = %d, want 404", p, got)
		}
	}
}

func TestUnixSocket_ModeStill0666(t *testing.T) {
	_, sock := startSocket(t)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o666 {
		t.Fatalf("socket mode = %o, want 666 (uid-mapped containers need it)", got)
	}
}

// blockingServer's Shutdown blocks until its context ends, like a server
// with a connection that never finishes.
type blockingServer struct {
	mu     sync.Mutex
	closed bool
}

func (b *blockingServer) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (b *blockingServer) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

func TestShutdownHTTPServers_ParallelWithinOneBudget(t *testing.T) {
	a, b := &blockingServer{}, &blockingServer{}
	start := time.Now()
	shutdownHTTPServers(context.Background(), 200*time.Millisecond, zerolog.Nop(), a, nil, b)
	elapsed := time.Since(start)
	if elapsed >= 380*time.Millisecond {
		t.Fatalf("two blocked servers took %v; a parallel shutdown is bounded by one 200ms budget", elapsed)
	}
	if !a.closed || !b.closed {
		t.Fatalf("both servers must be force-closed at the budget: a=%v b=%v", a.closed, b.closed)
	}
}

// seqLog hands out call positions under one lock: Phase 3 shuts the two
// servers down on separate goroutines.
type seqLog struct {
	mu sync.Mutex
	n  int
}

func (l *seqLog) next() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return l.n
}

type orderedExec struct {
	log *seqLog
	seq int
}

func (o *orderedExec) Shutdown(context.Context) error { o.seq = o.log.next(); return nil }

type orderedServer struct {
	log *seqLog
	mu  sync.Mutex
	seq int
}

func (o *orderedServer) Shutdown(context.Context) error {
	n := o.log.next()
	o.mu.Lock()
	o.seq = n
	o.mu.Unlock()
	return nil
}
func (o *orderedServer) Close() error { return nil }
func (o *orderedServer) at() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seq
}

func TestShutdown_SocketServerAfterExecutor(t *testing.T) {
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "0")
	log := &seqLog{}
	execSpy := &orderedExec{log: log}
	tcp := &orderedServer{log: log}
	sock := &orderedServer{log: log}
	c := &Container{
		Logger:             zerolog.Nop(),
		testExecShutdown:   execSpy,
		testHTTPShutdown:   tcp,
		testSocketShutdown: sock,
	}
	_ = c.shutdown()
	if sock.at() == 0 || tcp.at() == 0 {
		t.Fatalf("both servers must be shut down (tcp %d, socket %d)", tcp.at(), sock.at())
	}
	if execSpy.seq >= sock.at() || execSpy.seq >= tcp.at() {
		t.Fatalf("executor (seq %d) must pause before both servers close (tcp %d, socket %d)",
			execSpy.seq, tcp.at(), sock.at())
	}
}

// The matcher is exact: a pattern's prefix or extension does not match.
func TestAgentSocketRoutes_AllowlistIsExact(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/v1/chat/completions":                     true,
		"/api/v1/chat/completions/x":                   false,
		"/api/v1/projects/a/tasks/b/messages":          true,
		"/api/v1/projects/a/tasks/b/messages/c/answer": false,
		"/api/v1/projects/a/mcp/tools/call":            true,
		"/api/v1/projects/a/mcp/tools/list":            false,
		"/api/v1/projects/a/tasks/b/cancel":            false,
		"/api/v1/internal/trading-orders":              false,
	} {
		if got := allowedAgentSocketPath(p); got != want {
			t.Errorf("allowedAgentSocketPath(%q) = %v, want %v", p, got, want)
		}
	}
}

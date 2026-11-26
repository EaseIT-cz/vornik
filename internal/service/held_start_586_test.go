package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/starthold"
)

// GitHub #586: a daemon started with the start-held marker serves its
// listeners held and starts none of its work until an admin-class release;
// the release reloads files changed during the hold, then starts work
// (deploy idle gate design §13, §13.11). Found at: review (#581 D2 round 3:
// an upgrade was final at READY=1 because nothing could hold the new
// version from work). Regression test at: integration (a real container on
// real SQLite, through Container.Run and the daemon's real mux).

const held586Key = "sk-vornik-held-586-operator"

// fakeChat586 answers the chat provider's readiness probe, so the release's
// chat-provider wait returns at once.
func fakeChat586(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-model","object":"model"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

type held586 struct {
	c       *Container
	cfgPath string
	marker  string
	cancel  context.CancelFunc
	done    chan error
	once    sync.Once
}

// startHeld586 boots a real container with an autonomy-enabled project,
// retention on and a queued task, held by a marker, and runs it.
func startHeld586(t *testing.T) *held586 {
	t.Helper()
	cfgPath := isolatedConfigPath(t)
	dir := filepath.Join(filepath.Dir(cfgPath), "configs")
	files := map[string]string{
		"workflows/w1.md":       "---\nworkflowId: \"w1\"\ndisplayName: \"W\"\nentrypoint: \"go\"\nsteps:\n  go:\n    type: \"agent\"\n    role: \"coder\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n\n## Prompts\n\n### go\n\nDo the thing.\n",
		"swarms/s1.md":          "---\nswarmId: \"s1\"\ndisplayName: \"S\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n",
		"projects/auto894.yaml": project894,
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The release reloads config.yaml too (§13.11.3), so it must exist.
	if err := os.WriteFile(cfgPath, []byte("database:\n  driver: sqlite\n  path: \":memory:\"\napi:\n  auth_enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(cfgPath), starthold.FileName)
	body := `{"hold_id":"t586-` + time.Now().UTC().Format("20060102T150405") + `","created_at":"` +
		time.Now().UTC().Format(time.RFC3339) + `","reason":"test 586","deadline_seconds":1800}`
	if err := os.WriteFile(marker, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VORNIK_DRAIN_GRACE_SECONDS", "0")
	cfg := newComposerWiringTestConfig(t)
	cfg.Node.Profile = "" // all: workers, API, UI
	cfg.Chat.Endpoint = fakeChat586(t)
	cfg.Retention.Enabled = true
	cfg.Server.Address = "127.0.0.1:0"
	cfg.Server.UnixSocket = filepath.Join(t.TempDir(), "vornik.sock") // served by the no-op hook below
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{held586Key}
	cfg.Admin.Enabled = true
	cfg.Admin.AllowedKeys = []string{held586Key}
	c, err := newUnitTestContainer(t, cfg, cfgPath, WithStartHold(marker, starthold.Read(marker, time.Now())))
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	c.testAgentSocketServe = func(chan error) error { return nil }
	if err := c.repos.Tasks.Create(context.Background(), &persistence.Task{
		ID: "t586", ProjectID: "auto894", Status: persistence.TaskStatusQueued, MaxAttempts: 1,
		Payload: []byte(`{"workflow_id":"w1","context":{"prompt":"x"}}`)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &held586{c: c, cfgPath: cfgPath, marker: marker, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- c.Run(ctx) }()
	t.Cleanup(h.stop)
	waitFor586(t, 10*time.Second, "the held daemon to serve", func() bool { return c.heldServing.Load() })
	return h
}

func (h *held586) stop() {
	h.once.Do(func() {
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(30 * time.Second):
		}
	})
}

func waitFor586(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (h *held586) do(method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.c.HTTPServer.Handler.ServeHTTP(rec, req)
	return rec
}

func leaderLockRows586(t *testing.T, c *Container) int {
	t.Helper()
	var n int
	if err := c.DB.QueryRow(`SELECT COUNT(*) FROM daemon_leader_locks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func task586(t *testing.T, c *Container) *persistence.Task {
	t.Helper()
	got, err := c.repos.Tasks.Get(context.Background(), "t586")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHeldStart586_NoWorkWhileHeld_ReleaseReloadsAndStarts(t *testing.T) {
	h := startHeld586(t)
	c := h.c

	// Three scheduler poll intervals: nothing moves while held (review 9c0a
	// #1: the window is the scheduler's own interval, not a guess).
	if c.Scheduler == nil {
		t.Fatal("no scheduler was constructed; the test proves nothing")
	}
	time.Sleep(3*c.Scheduler.PollInterval() + 200*time.Millisecond)
	if st := task586(t, c).Status; st != persistence.TaskStatusQueued {
		t.Fatalf("held: task is %s, want QUEUED", st)
	}
	if n := leaderLockRows586(t, c); n != 0 {
		t.Fatalf("held: %d daemon_leader_locks rows; want 0, no elector may run while held", n)
	}
	if c.autonomyManager == nil {
		t.Fatal("no autonomy manager was constructed; the test proves nothing")
	}
	if n := c.autonomyManager.ActiveLoops(); n != 0 {
		t.Fatalf("held: %d autonomy loops, want 0", n)
	}
	if n := autonomyTicks894(t, c, 0); n != 0 {
		t.Fatalf("held: %d autonomy evaluations, want 0", n)
	}

	// The real mux, behind the gate.
	if w := h.do(http.MethodPost, "/api/v1/tasks", held586Key); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("held: task POST = %d, want 503 with Retry-After", w.Code)
	}
	if w := h.do(http.MethodGet, "/readyz", ""); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"status":"held"`) {
		t.Fatalf("held: /readyz = %d %s", w.Code, w.Body.String())
	}
	if w := h.do(http.MethodPost, "/api/v1/doctor?fix=true", held586Key); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("held: doctor --fix = %d, want 503", w.Code)
	}
	if w := h.do(http.MethodPost, "/api/v1/doctor", held586Key); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"start_held"`) {
		t.Fatalf("held: doctor = %d, want 200 with start_held", w.Code)
	}
	if w := h.do(http.MethodDelete, "/api/v1/daemon/hold", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("held: keyless release = %d, want 401", w.Code)
	}

	// Edited during the hold (operator answer 7): a second autonomy project.
	second := strings.ReplaceAll(project894, "auto894", "auto586b")
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.cfgPath), "configs", "projects", "auto586b.yaml"), []byte(second), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := h.do(http.MethodDelete, "/api/v1/daemon/hold", held586Key); w.Code != http.StatusOK {
		t.Fatalf("release = %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(h.marker); !os.IsNotExist(err) {
		t.Fatalf("the marker survived the release: %v", err)
	}
	if c.Registry.GetProject("auto586b") == nil {
		t.Fatal("the project added during the hold was not reloaded at the release")
	}
	if n := c.autonomyManager.ActiveLoops(); n != 2 {
		t.Fatalf("after the release: %d autonomy loops, want 2 (both projects)", n)
	}
	waitFor586(t, 10*time.Second, "the queued task to leave QUEUED", func() bool {
		return task586(t, c).Status != persistence.TaskStatusQueued
	})
	if n := leaderLockRows586(t, c); n == 0 {
		t.Fatal("after the release no elector took a lease; work did not start")
	}
	if w := h.do(http.MethodGet, "/readyz", ""); strings.Contains(w.Body.String(), `"status":"held"`) {
		t.Fatalf("after the release /readyz still says held: %s", w.Body.String())
	}
}

// §13.11.3: a reload that fails at release changes nothing and starts
// nothing; the operator sees why, and the release succeeds once fixed.
func TestHeldStart586_ReloadFailsAtRelease_StaysHeld(t *testing.T) {
	h := startHeld586(t)
	c := h.c
	// A valid new project and a config.yaml that does not parse, edited in
	// the same hold: the reload must take neither.
	good, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	added := filepath.Join(filepath.Dir(h.cfgPath), "configs", "projects", "auto586c.yaml")
	if err := os.WriteFile(added, []byte(strings.ReplaceAll(project894, "auto894", "auto586c")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfgPath, []byte("database: [this is not\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodDelete, "/api/v1/daemon/hold", held586Key)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "CONFIG_RELOAD_FAILED") ||
		!strings.Contains(w.Body.String(), "config.yaml") {
		t.Fatalf("release over a broken config.yaml = %d %s, want 500 CONFIG_RELOAD_FAILED naming the file", w.Code, w.Body.String())
	}
	if _, err := os.Stat(h.marker); err != nil {
		t.Fatalf("a refused release removed the marker: %v", err)
	}
	if !c.apiServer.IsHeld() || c.autonomyManager.ActiveLoops() != 0 || leaderLockRows586(t, c) != 0 {
		t.Fatal("a refused release started work")
	}
	if c.Registry.GetProject("auto586c") != nil {
		t.Fatal("a refused reload activated the staged project set: half-applied")
	}
	if g := h.do(http.MethodGet, "/api/v1/daemon/hold", held586Key); !strings.Contains(g.Body.String(), "last_error") {
		t.Fatalf("GET after the refusal does not name it: %s", g.Body.String())
	}
	if err := os.WriteFile(h.cfgPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if w := h.do(http.MethodDelete, "/api/v1/daemon/hold", held586Key); w.Code != http.StatusOK {
		t.Fatalf("release after the fix = %d %s", w.Code, w.Body.String())
	}
	if c.Registry.GetProject("auto586c") == nil {
		t.Fatal("the project added during the hold is not active after the release")
	}
}

// The activator does not start autonomy loops while held (§13.11.3): a
// reload before the release, from inside the process, leaves the loops off.
func TestHeldStart586_ReloadWhileHeld_StartsNoAutonomy(t *testing.T) {
	h := startHeld586(t)
	if err := h.c.ConfigReloader.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := h.c.autonomyManager.ActiveLoops(); n != 0 {
		t.Fatalf("a reload while held started %d autonomy loops", n)
	}
}

// L3 and L5 serve nothing while held (§13.8 item 3).
func TestHeldStart586_AgentSocketAndRelayRefuseEverything(t *testing.T) {
	h := startHeld586(t)
	c := h.c
	if c.AgentSocketServer == nil {
		t.Fatal("no agent socket server; the test proves nothing")
	}
	for _, p := range []string{"/api/v1/chat/completions", "/v1/chat/completions", "/api/v1/agent/host-approvals"} {
		rec := httptest.NewRecorder()
		c.AgentSocketServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("agent socket %s held = %d, want 503", p, rec.Code)
		}
	}
	relay := c.relayIngressHandler()
	for _, p := range []string{"/internal/v1/webhook-relay", "/internal/v1/node-heartbeat"} {
		rec := httptest.NewRecorder()
		relay.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("relay ingress %s held = %d, want 503", p, rec.Code)
		}
	}
	n, err := c.repos.Tasks.List(context.Background(), persistence.TaskFilter{PageSize: 10})
	if err != nil || len(n) != 1 {
		t.Fatalf("tasks after the refused relay posts: %d (%v), want only the seeded one", len(n), err)
	}
}

// A SIGHUP while held is logged and deferred: the release is the reload
// (§13.2, §13.11.3).
func TestHeldStart586_SIGHUPWhileHeldDoesNotReload(t *testing.T) {
	h := startHeld586(t)
	if h.c.reloadOnSignal() {
		t.Fatal("a SIGHUP while held reloaded; it must be deferred to the release")
	}
	if w := h.do(http.MethodDelete, "/api/v1/daemon/hold", held586Key); w.Code != http.StatusOK {
		t.Fatalf("release = %d", w.Code)
	}
	if !h.c.reloadOnSignal() {
		t.Fatal("a SIGHUP after the release did not reload")
	}
}

// service.Run reads the marker from VORNIK_DATA_DIR, or beside the config
// file when it is unset (operator answer 3).
func TestHeldStart586_ServiceRunReadsTheMarkerWhereItLives(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	body := []byte(`{"hold_id":"x","created_at":"2026-11-24T10:00:00Z"}`)
	t.Setenv("VORNIK_DATA_DIR", data)
	if p, h := readStartHold(filepath.Join(cfgDir, "config.yaml"), time.Now()); h.Held || p != filepath.Join(data, starthold.FileName) {
		t.Fatalf("no marker: %s %+v", p, h)
	}
	if err := os.WriteFile(filepath.Join(data, starthold.FileName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, h := readStartHold(filepath.Join(cfgDir, "config.yaml"), time.Now()); !h.Held || h.HoldID != "x" {
		t.Fatalf("marker in the data dir: %+v", h)
	}
	t.Setenv("VORNIK_DATA_DIR", "")
	if err := os.WriteFile(filepath.Join(cfgDir, starthold.FileName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if p, h := readStartHold(filepath.Join(cfgDir, "config.yaml"), time.Now()); !h.Held || p != filepath.Join(cfgDir, starthold.FileName) {
		t.Fatalf("marker beside the config: %s %+v", p, h)
	}
}

// A held daemon that stops (or crashes) without a release comes back held:
// the marker survives the process (§13.1).
func TestHeldStart586_StopWithoutRelease_ComesBackHeld(t *testing.T) {
	h := startHeld586(t)
	if n := leaderLockRows586(t, h.c); n != 0 {
		t.Fatalf("a held daemon wrote %d leader locks", n)
	}
	h.stop()
	if hold := starthold.Read(h.marker, time.Now()); !hold.Held {
		t.Fatal("the marker did not survive a stop without a release")
	}
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #586: a held start serves the listeners behind one allowlist gate
// and starts no work until an admin-class DELETE /api/v1/daemon/hold
// releases it. The release reloads the config files, removes the marker,
// then starts work (deploy idle gate design §13.4, §13.11.3).
// Found at: review (#581 D2 round 3: the drain cannot hold a new version
// from work, so an upgrade was final at READY=1). Regression test at: unit
// (this file), integration and executor-e2e in internal/service.

// holdAuditRepo records admin-audit rows and answers List by action and
// target prefix, as the SQL repositories do.
type holdAuditRepo struct {
	mu      sync.Mutex
	rows    []persistence.AdminAuditEntry
	listErr error
}

func (r *holdAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, *e)
	return nil
}

func (r *holdAuditRepo) List(_ context.Context, f persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []*persistence.AdminAuditEntry
	for i := range r.rows {
		e := r.rows[i]
		if (f.Action == "" || e.Action == f.Action) && strings.HasPrefix(e.Target, f.TargetPrefix) {
			out = append(out, &e)
		}
	}
	return out, nil
}

func (r *holdAuditRepo) count(action string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.rows {
		if e.Action == action {
			n++
		}
	}
	return n
}

// fakeReleaser records the release's calls in order.
type fakeReleaser struct {
	mu         sync.Mutex
	calls      []string
	reloadErr  error
	removeErr  error
	startErr   error
	restoreErr error
	startGate  chan struct{} // when set, StartWork waits on it
	started    chan struct{} // closed when StartWork is entered
	// failedSaw is the response the handler had written when
	// WorkStartFailed ran (the 500 goes out before the error reaches errCh).
	rec       *httptest.ResponseRecorder
	failedSaw int
	failedErr error
}

func (f *fakeReleaser) record(c string) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *fakeReleaser) ReloadConfig() error { f.record("reload"); return f.reloadErr }
func (f *fakeReleaser) RemoveMarker() error { f.record("remove"); return f.removeErr }
func (f *fakeReleaser) RestoreMarker() error {
	f.record("restore")
	return f.restoreErr
}

func (f *fakeReleaser) StartWork() error {
	f.record("start")
	if f.started != nil {
		close(f.started)
	}
	if f.startGate != nil {
		<-f.startGate
	}
	return f.startErr
}

func (f *fakeReleaser) WorkStartFailed(err error) {
	f.record("failed")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failedErr = err
	if f.rec != nil && f.rec.Flushed {
		f.failedSaw = f.rec.Code
	}
}

func (f *fakeReleaser) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeReleaser) startCount() int {
	n := 0
	for _, x := range f.list() {
		if x == "start" {
			n++
		}
	}
	return n
}

var hold586Since = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func holdInfo586() HoldInfo {
	return HoldInfo{HoldID: "a-b-1764000000", Reason: "managed-upgrade a->b",
		Since: hold586Since, Deadline: hold586Since.Add(30 * time.Minute)}
}

type holdRig struct {
	s      *Server
	rel    *fakeReleaser
	audit  *holdAuditRepo
	timers *fakeTimers
	now    time.Time
	alerts []string
	mu     sync.Mutex
}

func newHoldRig(t *testing.T) *holdRig {
	t.Helper()
	rig := &holdRig{rel: &fakeReleaser{}, audit: &holdAuditRepo{}, timers: &fakeTimers{}, now: hold586Since.Add(time.Minute)}
	rig.s = newHoldServer(rig, holdInfo586())
	return rig
}

func newHoldServer(rig *holdRig, info HoldInfo) *Server {
	alert := func(_ context.Context, subject, body string) error {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.alerts = append(rig.alerts, subject+": "+body)
		return nil
	}
	s := NewServer(
		WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{drainAdminKey}}),
		WithAdminAuditRepository(rig.audit),
		WithDrainClock(func() time.Time { rig.mu.Lock(); defer rig.mu.Unlock(); return rig.now }),
		WithStartHold(info, rig.rel, alert),
	)
	s.holdAfterFunc = rig.timers.afterFunc
	return s
}

func (rig *holdRig) setNow(t time.Time) {
	rig.mu.Lock()
	rig.now = t
	rig.mu.Unlock()
}

func (rig *holdRig) alertCount() int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return len(rig.alerts)
}

func holdCall(s *Server, method, key string, rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/daemon/hold", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	if key != "" {
		ctx = context.WithValue(ctx, apiKeyKey, key)
	}
	if rec == nil {
		rec = httptest.NewRecorder()
	}
	s.DaemonHold(rec, req.WithContext(ctx))
	return rec
}

func holdErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Error.Code
}

func TestHold586_ReleaseOrder_ReloadRemoveStart(t *testing.T) {
	rig := newHoldRig(t)
	if !rig.s.IsHeld() {
		t.Fatal("WithStartHold did not hold")
	}
	w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("release = %d %s", w.Code, w.Body.String())
	}
	if got := strings.Join(rig.rel.list(), ","); got != "reload,remove,start" {
		t.Fatalf("release order = %s, want reload,remove,start", got)
	}
	if rig.s.IsHeld() {
		t.Fatal("still held after a 200 release")
	}
	if n := rig.audit.count("daemon.release"); n != 1 {
		t.Fatalf("daemon.release rows = %d, want 1", n)
	}
	// A second DELETE on an unheld daemon changes nothing.
	w = holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"held":false`) {
		t.Fatalf("second release = %d %s", w.Code, w.Body.String())
	}
	if rig.rel.startCount() != 1 || rig.audit.count("daemon.release") != 1 {
		t.Fatalf("a release on an unheld daemon did something: %v", rig.rel.list())
	}
}

func TestHold586_ConcurrentReleases_StartWorkOnce(t *testing.T) {
	rig := newHoldRig(t)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = holdCall(rig.s, http.MethodDelete, drainAdminKey, nil).Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("release %d = %d", i, c)
		}
	}
	if n := rig.rel.startCount(); n != 1 {
		t.Fatalf("startWork ran %d times under 8 concurrent releases, want 1", n)
	}
}

func TestHold586_MarkerNotRemoved_500_NothingStarted(t *testing.T) {
	rig := newHoldRig(t)
	rig.rel.removeErr = errors.New("permission denied")
	w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if w.Code != http.StatusInternalServerError || holdErrCode(t, w) != "MARKER_NOT_REMOVED" {
		t.Fatalf("release = %d %s, want 500 MARKER_NOT_REMOVED", w.Code, w.Body.String())
	}
	if rig.rel.startCount() != 0 || !rig.s.IsHeld() {
		t.Fatalf("work started or hold cleared: %v held=%v", rig.rel.list(), rig.s.IsHeld())
	}
	if rig.audit.count("daemon.release") != 0 {
		t.Fatal("a refused release wrote an audit row")
	}
}

// §13.11.3: a reload that fails changes nothing, says why, and the release
// can be retried once the files are fixed.
func TestHold586_ReloadFails_StaysHeld_RetryReleases(t *testing.T) {
	rig := newHoldRig(t)
	rig.rel.reloadErr = errors.New("validation failed: project p1: unknown swarm s9")
	w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if w.Code != http.StatusInternalServerError || holdErrCode(t, w) != "CONFIG_RELOAD_FAILED" {
		t.Fatalf("release = %d %s, want 500 CONFIG_RELOAD_FAILED", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown swarm s9") {
		t.Fatalf("the reason is not in the answer: %s", w.Body.String())
	}
	if got := strings.Join(rig.rel.list(), ","); got != "reload" {
		t.Fatalf("calls after a failed reload = %s, want reload only (marker kept, nothing started)", got)
	}
	if !rig.s.IsHeld() || rig.audit.count("daemon.release") != 0 {
		t.Fatal("a refused reload released or wrote an audit row")
	}
	g := holdCall(rig.s, http.MethodGet, drainAdminKey, nil)
	if !strings.Contains(g.Body.String(), "unknown swarm s9") {
		t.Fatalf("GET does not report last_error: %s", g.Body.String())
	}
	// Fixed: the retry releases and clears last_error.
	rig.rel.reloadErr = nil
	if w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil); w.Code != http.StatusOK {
		t.Fatalf("retry = %d %s", w.Code, w.Body.String())
	}
	g = holdCall(rig.s, http.MethodGet, drainAdminKey, nil)
	if strings.Contains(g.Body.String(), "unknown swarm") {
		t.Fatalf("last_error survived a successful release: %s", g.Body.String())
	}
}

func TestHold586_WorkStartFails_RestoresMarker_500First_Then409(t *testing.T) {
	rig := newHoldRig(t)
	rig.rel.startErr = errors.New("failed to start scheduler: boom")
	rig.rel.startGate = make(chan struct{})
	rig.rel.started = make(chan struct{})
	rec := httptest.NewRecorder()
	rig.rel.rec = rec
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- holdCall(rig.s, http.MethodDelete, drainAdminKey, rec) }()
	<-rig.rel.started
	// A second release arrives while the first holds the mutex.
	waiting := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiting <- holdCall(rig.s, http.MethodDelete, drainAdminKey, nil) }()
	close(rig.rel.startGate)
	w := <-first
	if w.Code != http.StatusInternalServerError || holdErrCode(t, w) != "WORK_START_FAILED" {
		t.Fatalf("release = %d %s, want 500 WORK_START_FAILED", w.Code, w.Body.String())
	}
	if rig.rel.failedSaw != http.StatusInternalServerError {
		t.Fatalf("the error reached errCh before the 500 was flushed (saw %d)", rig.rel.failedSaw)
	}
	if got := strings.Join(rig.rel.list()[:4], ","); got != "reload,remove,start,restore" {
		t.Fatalf("calls = %v, want reload,remove,start,restore then failed", rig.rel.list())
	}
	if w2 := <-waiting; w2.Code != http.StatusConflict || holdErrCode(t, w2) != "RELEASE_FAILED" {
		t.Fatalf("waiting release = %d %s, want 409 RELEASE_FAILED", w2.Code, w2.Body.String())
	}
	if w3 := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil); w3.Code != http.StatusConflict {
		t.Fatalf("later release = %d, want 409", w3.Code)
	}
	if n := rig.rel.startCount(); n != 1 {
		t.Fatalf("startWork ran %d times, want exactly 1", n)
	}
	if !rig.s.IsHeld() {
		t.Fatal("the hold was cleared after a failed work start")
	}
	g := holdCall(rig.s, http.MethodGet, drainAdminKey, nil)
	if !strings.Contains(g.Body.String(), `"release_failed":true`) {
		t.Fatalf("GET = %s, want release_failed true", g.Body.String())
	}
	r := httptest.NewRecorder()
	rig.s.Readyz(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if !strings.Contains(r.Body.String(), `"status":"held"`) {
		t.Fatalf("/readyz after a failed release = %s, want held", r.Body.String())
	}
}

func TestHold586_ShuttingDown_409(t *testing.T) {
	rig := newHoldRig(t)
	rig.s.BeginShutdownDrain()
	w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if w.Code != http.StatusConflict || holdErrCode(t, w) != "SHUTTING_DOWN" {
		t.Fatalf("release while shutting down = %d %s", w.Code, w.Body.String())
	}
	if len(rig.rel.list()) != 0 {
		t.Fatalf("release while shutting down did something: %v", rig.rel.list())
	}
}

func TestHold586_NonAdmin_403(t *testing.T) {
	rig := newHoldRig(t)
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if w := holdCall(rig.s, m, "sk-not-admin", nil); w.Code != http.StatusForbidden {
			t.Fatalf("%s non-admin = %d, want 403", m, w.Code)
		}
	}
	if w := holdCall(rig.s, http.MethodPost, drainAdminKey, nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", w.Code)
	}
	if len(rig.rel.list()) != 0 {
		t.Fatal("a refused call did something")
	}
}

func TestHold586_GetReportsTheHold(t *testing.T) {
	rig := newHoldRig(t)
	w := holdCall(rig.s, http.MethodGet, drainAdminKey, nil)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	if body["held"] != true || body["overdue"] != false || body["reason"] != "managed-upgrade a->b" ||
		body["hold_id"] != "a-b-1764000000" || body["since"] != "2026-11-24T10:00:00Z" || body["deadline"] != "2026-11-24T10:30:00Z" {
		t.Fatalf("GET body = %v", body)
	}
}

func TestHold586_Readyz_HeldThenOverdue_DrainWins(t *testing.T) {
	rig := newHoldRig(t)
	rig.s.readinessChecks = append(rig.s.readinessChecks, ReadinessCheck{Name: "chat_provider", Check: func(context.Context) error { return nil }})
	read := func() (int, map[string]any) {
		r := httptest.NewRecorder()
		rig.s.Readyz(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var body map[string]any
		_ = json.Unmarshal(r.Body.Bytes(), &body)
		return r.Code, body
	}
	code, body := read()
	if code != http.StatusServiceUnavailable || body["status"] != "held" || body["overdue"] != false || body["checks"] == nil {
		t.Fatalf("held /readyz = %d %v", code, body)
	}
	rig.setNow(hold586Since.Add(31 * time.Minute))
	if _, body = read(); body["overdue"] != true {
		t.Fatalf("past the deadline /readyz = %v, want overdue", body)
	}
	rig.s.BeginShutdownDrain()
	if _, body = read(); body["status"] != "draining" {
		t.Fatalf("draining and held /readyz = %v, want draining", body)
	}
}

// §13.6: the deadline alerts once per hold_id, across processes, through
// the audit table; the clock and timer are injected, never slept.
func TestHold586_Overdue_AlertsOncePerHoldID(t *testing.T) {
	rig := newHoldRig(t)
	rig.s.ArmHoldDeadline()
	if rig.timers.count() != 1 || rig.timers.ds[0] != 29*time.Minute {
		t.Fatalf("armed %d timers (%v), want one of 29m", rig.timers.count(), rig.timers.ds)
	}
	rig.setNow(hold586Since.Add(30 * time.Minute))
	rig.timers.fire(0)
	if rig.alertCount() != 1 || rig.audit.count("daemon.hold.overdue") != 1 {
		t.Fatalf("first process: alerts %d, rows %d; want 1 and 1", rig.alertCount(), rig.audit.count("daemon.hold.overdue"))
	}
	if !rig.s.IsHeld() {
		t.Fatal("an overdue hold released itself")
	}
	// A second process (a restart) with the same hold_id, past the deadline.
	rig.timers = &fakeTimers{}
	s2 := newHoldServer(rig, holdInfo586())
	s2.ArmHoldDeadline()
	if rig.timers.count() != 1 || rig.timers.ds[0] != 0 {
		t.Fatalf("restarted past the deadline: timers %v, want one at 0", rig.timers.ds)
	}
	rig.timers.fire(0)
	if rig.alertCount() != 1 || rig.audit.count("daemon.hold.overdue") != 1 {
		t.Fatalf("second process: alerts %d, rows %d; want still 1 and 1", rig.alertCount(), rig.audit.count("daemon.hold.overdue"))
	}
	// A different hold alerts again.
	other := holdInfo586()
	other.HoldID = "c-d-1764000001"
	rig.timers = &fakeTimers{}
	s3 := newHoldServer(rig, other)
	s3.ArmHoldDeadline()
	rig.timers.fire(0)
	if rig.alertCount() != 2 {
		t.Fatalf("a new hold_id: alerts %d, want 2", rig.alertCount())
	}
}

func TestHold586_Overdue_LookupFails_AlertsAnyway(t *testing.T) {
	rig := newHoldRig(t)
	rig.audit.listErr = errors.New("db down")
	rig.setNow(hold586Since.Add(time.Hour))
	rig.s.ArmHoldDeadline()
	rig.timers.fire(0)
	if rig.alertCount() != 1 {
		t.Fatalf("alerts = %d, want 1 (a repeated alert is accepted, a lost one is not)", rig.alertCount())
	}
}

func TestHold586_ReleasedBeforeDeadline_NoAlert(t *testing.T) {
	rig := newHoldRig(t)
	rig.s.ArmHoldDeadline()
	if w := holdCall(rig.s, http.MethodDelete, drainAdminKey, nil); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	rig.setNow(hold586Since.Add(time.Hour))
	rig.timers.fire(0)
	if rig.alertCount() != 0 {
		t.Fatal("a released hold alerted at its deadline")
	}
}

// ----- the gate (§13.2, §13.8 item 3) -----

func gateCall(h http.Handler, method, target string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w.Code
}

func TestHoldGate586_Allowlist(t *testing.T) {
	held := true
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	gate := HoldGate(func() bool { return held }, HoldAllowed)(inner)
	allowed := []struct{ method, target string }{
		{"GET", "/livez"}, {"GET", "/healthz"}, {"GET", "/readyz"}, {"GET", "/health/live"},
		{"GET", "/health/ready"}, {"GET", "/metrics"},
		{"POST", "/api/v1/doctor"}, {"POST", "/api/v1/doctor?fix=false"},
		{"GET", "/api/v1/doctor/features"}, {"GET", "/api/v1/doctor/features/memory"},
		{"GET", "/api/v1/daemon/hold"}, {"DELETE", "/api/v1/daemon/hold"},
	}
	for _, a := range allowed {
		if c := gateCall(gate, a.method, a.target); c != http.StatusTeapot {
			t.Errorf("%s %s held = %d, want it through the gate", a.method, a.target, c)
		}
	}
	refused := []struct{ method, target string }{
		{"POST", "/api/v1/doctor?fix=true"}, {"POST", "/api/v1/doctor/ack"},
		{"POST", "/api/v1/tasks"}, {"GET", "/api/v1/tasks"}, {"POST", "/readyz"},
		{"POST", "/api/v1/daemon/hold"}, {"GET", "/ui/"}, {"POST", "/api/v1/config/reload"},
		{"GET", "/api/v1/daemon//hold"}, {"DELETE", "/api/v1/daemon/drain"},
		// Unclean paths that start like an allowed one (review 252e #1).
		{"GET", "/api/v1/doctor/features/../../tasks"}, {"GET", "/api/v1/doctor/features/./x"},
		{"GET", "/api/v1/doctor/features//x"}, {"GET", "/readyz/"},
	}
	for _, r := range refused {
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, httptest.NewRequest(r.method, r.target, nil))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "30" ||
			!strings.Contains(w.Body.String(), `"status":"held"`) {
			t.Errorf("%s %s held = %d %q %s, want 503 held with Retry-After 30", r.method, r.target, w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	}
	held = false
	if c := gateCall(gate, "POST", "/api/v1/tasks"); c != http.StatusTeapot {
		t.Fatalf("released gate = %d, want everything through", c)
	}
	// The no-allowlist form (relay ingress, agent socket) refuses everything.
	held = true
	all := HoldGate(func() bool { return held }, nil)(inner)
	if c := gateCall(all, "GET", "/readyz"); c != http.StatusServiceUnavailable {
		t.Fatalf("refuse-all gate let /readyz through: %d", c)
	}
}

// routePatterns extracts every string-literal pattern registered with
// Handle or HandleFunc in the given files.
func routePatterns(t *testing.T, paths ...string) (lits []string, nonLiteral int) {
	t.Helper()
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				nonLiteral++
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err == nil && !seen[v] {
				seen[v] = true
				lits = append(lits, v)
			}
			return true
		})
	}
	sort.Strings(lits)
	return lits, nonLiteral
}

// Every registered route outside the allowlist answers 503 while held, for
// every method. The denominator is printed: an allowlist that passes
// because it examined nothing would say so.
func TestHoldGate586_WalksEveryRegisteredRoute(t *testing.T) {
	pats, nonLit := routePatterns(t, "routes.go", "../service/container_http.go")
	if len(pats) < 100 {
		t.Fatalf("only %d route patterns found; the walk examines nothing useful", len(pats))
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	gate := HoldGate(func() bool { return true }, HoldAllowed)(inner)
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	allowed := 0
	var through []string
	for _, p := range pats {
		// A Go 1.22 pattern may carry a method ("GET /x") and wildcards
		// ("{id}"); walk its path, with each wildcard as a plain segment.
		target := p
		if i := strings.Index(target, " "); i > 0 {
			target = strings.TrimSpace(target[i+1:])
		}
		if !strings.HasPrefix(target, "/") {
			target = "/" + target
		}
		target = strings.ReplaceAll(strings.ReplaceAll(target, "{", "x"), "}", "x")
		for _, m := range methods {
			if gateCall(gate, m, target) != http.StatusServiceUnavailable {
				allowed++
				through = append(through, m+" "+target)
			}
		}
	}
	t.Logf("held gate: %d route patterns examined (%d registered by a non-literal, not walked) x %d methods; %d let through: %v",
		len(pats), nonLit, len(methods), allowed, through)
	want := map[string]bool{
		"GET /livez": true, "GET /healthz": true, "GET /readyz": true, "GET /health/live": true,
		"GET /health/ready": true, "GET /metrics": true, "POST /api/v1/doctor": true,
		"GET /api/v1/doctor/features": true,
		"GET /api/v1/daemon/hold":     true, "DELETE /api/v1/daemon/hold": true,
	}
	for _, x := range through {
		// A feature page (a wildcard under /api/v1/doctor/features/) is a
		// read and is on the allowlist by prefix.
		if !want[x] && !strings.HasPrefix(x, "GET /api/v1/doctor/features/") {
			t.Errorf("held gate let %s through; it is not on the allowlist", x)
		}
	}
	if !strings.Contains(fmt.Sprint(pats), "/api/v1/daemon/hold") {
		t.Error("the release route is not registered in routes.go")
	}
}

// Doctor's start_held: WARNING while held, ERROR when overdue, OK once
// released (§13.3).
func TestHold586_DoctorStartHeld(t *testing.T) {
	rig := newHoldRig(t)
	h := &DoctorHandlers{server: rig.s}
	if c := h.checkStartHeld(); c.Status != "WARNING" || !strings.Contains(c.Message, "a-b-1764000000") {
		t.Fatalf("held: %+v", c)
	}
	rig.setNow(hold586Since.Add(time.Hour))
	if c := h.checkStartHeld(); c.Status != "ERROR" || !strings.HasPrefix(c.Message, "OVERDUE") {
		t.Fatalf("overdue: %+v", c)
	}
	rig.rel.reloadErr = errors.New("bad file")
	holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if c := h.checkStartHeld(); len(c.Items) == 0 || !strings.Contains(c.Items[0], "bad file") {
		t.Fatalf("refused release not named: %+v", c)
	}
	rig.rel.reloadErr = nil
	holdCall(rig.s, http.MethodDelete, drainAdminKey, nil)
	if c := h.checkStartHeld(); c.Status != "OK" {
		t.Fatalf("released: %+v", c)
	}
	if c := (&DoctorHandlers{}).checkStartHeld(); c.Status != "OK" {
		t.Fatalf("no server: %+v", c)
	}
}

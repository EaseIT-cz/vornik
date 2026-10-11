package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #290 (2026-10-08): the deploy idle gate read the chat-proxy count as
// 0 and only then did SIGTERM set the drain bit, so a request that started in
// between was admitted, counted, and cut at shutdown Phase 3 (the #97 502 in
// a smaller window). An admin-class drain verb now sets the bit first; the
// gate reads the count after it. Design:
// https://docs.vornik.io §9.

// drainAuditRepo records admin-audit rows; safe for the expiry goroutine.
type drainAuditRepo struct {
	mu   sync.Mutex
	rows []persistence.AdminAuditEntry
}

func (r *drainAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, *e)
	return nil
}

func (r *drainAuditRepo) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

func (r *drainAuditRepo) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.rows))
	for _, e := range r.rows {
		out = append(out, e.Action)
	}
	return out
}

// fakeTimers captures the expiry callbacks drainAfterFunc arms, so a test
// fires expiry deterministically instead of sleeping.
type fakeTimers struct {
	mu    sync.Mutex
	fns   []func()
	ds    []time.Duration
	calls int
}

func (f *fakeTimers) fired() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTimers) afterFunc(d time.Duration, fn func()) *time.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fns = append(f.fns, fn)
	f.ds = append(f.ds, d)
	// A real timer far in the future, so Stop has something to stop and the
	// callback never runs by itself.
	return time.AfterFunc(time.Hour, func() {})
}

func (f *fakeTimers) fire(i int) {
	f.mu.Lock()
	fn := f.fns[i]
	f.mu.Unlock()
	fn()
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
}

func (f *fakeTimers) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fns)
}

const drainAdminKey = "sk-operator"

func drainServer(t *testing.T, opts ...ServerOption) (*Server, *fakeTimers) {
	t.Helper()
	opts = append([]ServerOption{WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{drainAdminKey}})}, opts...)
	s := NewServer(opts...)
	ft := &fakeTimers{}
	s.drainAfterFunc = ft.afterFunc
	return s, ft
}

func drainCall(t *testing.T, s *Server, method, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/api/v1/daemon/drain", nil)
	} else {
		req = httptest.NewRequest(method, "/api/v1/daemon/drain", strings.NewReader(body))
	}
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	if key != "" {
		ctx = context.WithValue(ctx, apiKeyKey, key)
	}
	w := httptest.NewRecorder()
	s.DaemonDrain(w, req.WithContext(ctx))
	return w
}

type drainBody struct {
	Draining     bool   `json:"draining"`
	ShuttingDown bool   `json:"shutting_down"`
	Since        string `json:"since"`
	ExpiresAt    string `json:"expires_at"`
	ChatProxy    int64  `json:"chat_proxy"`
	Total        int64  `json:"total"`
}

func decodeDrain(t *testing.T, w *httptest.ResponseRecorder) drainBody {
	t.Helper()
	var b drainBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return b
}

// The API half of the fix (review ef08 #1): a request admitted before the
// bit is counted in the drain answer itself and in every read after it, a
// request after the bit is refused, and the post-drain read reaches 0 only
// when the admitted one ends. The ORDER (drain, then read) is the gate's,
// pinned by the call-sequence cases in scripts/deploy-idle-gate_test.sh.
func TestDrain_NewRequestRefusedInflightCompletesGateSeesZero(t *testing.T) {
	prov := newBlockingProvider()
	s, _ := drainServer(t, WithChatProvider(prov))
	s.logger = zerolog.Nop()
	h := s.trackLongRequest(s.ChatCompletions)

	done := make(chan int)
	go func() {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatBody()))
		done <- w.Code
	}()
	<-prov.entered

	w := drainCall(t, s, http.MethodPost, drainAdminKey, `{"ttl_seconds":60}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST drain = %d %s, want 200", w.Code, w.Body.String())
	}
	if b := decodeDrain(t, w); !b.Draining || b.Total != 1 || b.ExpiresAt == "" {
		t.Fatalf("drain body = %+v, want draining with the open request counted and a deadline", b)
	}

	// A request after the bit is refused and never counted.
	nw := httptest.NewRecorder()
	h(nw, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatBody()))
	if nw.Code != http.StatusServiceUnavailable || !strings.Contains(nw.Body.String(), "DRAINING") {
		t.Fatalf("new request under drain = %d %s, want 503 DRAINING", nw.Code, nw.Body.String())
	}
	if got := s.InflightChatProxy(); got != 1 {
		t.Fatalf("in-flight = %d, want 1 (the refused request must not count)", got)
	}

	// The one in flight before the bit completes.
	close(prov.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the open request = %d, want 200", code)
	}

	// The gate's read after the bit now answers 0, still draining.
	iw := inflightRequest(t, s, drainAdminKey)
	if iw.Code != http.StatusOK || !strings.Contains(iw.Body.String(), `"total":0`) || !strings.Contains(iw.Body.String(), `"draining":true`) {
		t.Fatalf("in-flight read under drain = %d %s, want total 0 and draining true", iw.Code, iw.Body.String())
	}
}

func TestDrain_UndrainClearsTheBit(t *testing.T) {
	s, _ := drainServer(t, WithChatProvider(okStub()))
	if w := drainCall(t, s, http.MethodPost, drainAdminKey, ""); w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body.String())
	}
	if !s.IsDraining() {
		t.Fatal("POST did not set the bit")
	}
	w := drainCall(t, s, http.MethodDelete, drainAdminKey, "")
	if w.Code != http.StatusOK || decodeDrain(t, w).Draining {
		t.Fatalf("DELETE = %d %s, want 200 draining false", w.Code, w.Body.String())
	}
	if s.IsDraining() {
		t.Fatal("DELETE did not clear the bit")
	}
	cw := httptest.NewRecorder()
	s.trackLongRequest(s.ChatCompletions)(cw, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatBody()))
	if cw.Code != http.StatusOK {
		t.Fatalf("a request after undrain = %d, want 200", cw.Code)
	}
}

func TestDrain_TimeoutClearsTheBit(t *testing.T) {
	audit := &drainAuditRepo{}
	s, ft := drainServer(t, WithAdminAuditRepository(audit))
	if w := drainCall(t, s, http.MethodPost, drainAdminKey, `{"ttl_seconds":120}`); w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body.String())
	}
	if ft.count() != 1 || ft.ds[0] != 120*time.Second {
		t.Fatalf("armed timers = %d %v, want one of 120s", ft.count(), ft.ds)
	}
	// A renewal arms a new generation; the first timer firing late is stale.
	if w := drainCall(t, s, http.MethodPost, drainAdminKey, `{"ttl_seconds":120}`); w.Code != http.StatusOK {
		t.Fatalf("renewal = %d", w.Code)
	}
	ft.fire(0)
	if !s.IsDraining() {
		t.Fatal("a stale expiry (before the renewal) cleared the bit")
	}
	ft.fire(1)
	if s.IsDraining() {
		t.Fatal("the current expiry did not clear the bit")
	}
	if s.DrainedFor() != 0 {
		t.Fatal("DrainedFor after expiry must be 0")
	}
	got := audit.actions()
	if len(got) == 0 || got[len(got)-1] != "daemon.drain.expired" {
		t.Fatalf("audit actions = %v, want the last to be daemon.drain.expired", got)
	}
}

func TestDrain_IsIdempotentAndRenews(t *testing.T) {
	s, _ := drainServer(t)
	base := time.Now()
	s.drainNow = func() time.Time { return base }
	first := decodeDrain(t, drainCall(t, s, http.MethodPost, drainAdminKey, `{"ttl_seconds":60}`))
	s.drainNow = func() time.Time { return base.Add(10 * time.Second) }
	second := decodeDrain(t, drainCall(t, s, http.MethodPost, drainAdminKey, `{"ttl_seconds":60}`))
	if first.Since == "" || first.Since != second.Since {
		t.Fatalf("since moved on renewal: %q then %q", first.Since, second.Since)
	}
	e1, _ := time.Parse(time.RFC3339, first.ExpiresAt)
	e2, _ := time.Parse(time.RFC3339, second.ExpiresAt)
	if !e2.After(e1) {
		t.Fatalf("expires_at did not move: %s then %s", first.ExpiresAt, second.ExpiresAt)
	}
	for i := 0; i < 2; i++ {
		if w := drainCall(t, s, http.MethodDelete, drainAdminKey, ""); w.Code != http.StatusOK {
			t.Fatalf("DELETE #%d = %d %s, want 200", i+1, w.Code, w.Body.String())
		}
	}
}

func TestDrain_NonAdminKeyIs403(t *testing.T) {
	audit := &drainAuditRepo{}
	s, _ := drainServer(t, WithAdminAuditRepository(audit))
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		for _, key := range []string{"sk-tenant", ""} {
			w := drainCall(t, s, m, key, "")
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "ADMIN_SCOPE_REQUIRED") {
				t.Fatalf("%s with key %q = %d %s, want 403 ADMIN_SCOPE_REQUIRED", m, key, w.Code, w.Body.String())
			}
		}
	}
	if s.IsDraining() {
		t.Fatal("a refused caller set the bit")
	}
	if n := len(audit.actions()); n != 0 {
		t.Fatalf("a refused call wrote %d audit row(s)", n)
	}
}

// Through the real router and AuthMiddleware: admin key drains, a tenant key
// is refused, and the route is registered in both editions' API region.
func TestDrain_ThroughRouter(t *testing.T) {
	s, _ := drainServer(t)
	cfg := &config.Config{API: config.APIConfig{AuthEnabled: true, APIKeys: []string{drainAdminKey, "sk-tenant"}}}
	h := SetupRoutes(s, cfg)
	call := func(method, key string) int {
		req := httptest.NewRequest(method, "/api/v1/daemon/drain", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if c := call(http.MethodPost, "sk-tenant"); c != http.StatusForbidden {
		t.Fatalf("tenant POST = %d, want 403", c)
	}
	// No key: AuthMiddleware refuses before the handler (review e381 #6).
	if c := call(http.MethodPost, ""); c != http.StatusUnauthorized || s.IsDraining() {
		t.Fatalf("keyless POST = %d draining=%t, want 401 and clear", c, s.IsDraining())
	}
	if c := call(http.MethodPost, drainAdminKey); c != http.StatusOK || !s.IsDraining() {
		t.Fatalf("admin POST = %d draining=%t, want 200 and drained", c, s.IsDraining())
	}
	if c := call(http.MethodDelete, drainAdminKey); c != http.StatusOK || s.IsDraining() {
		t.Fatalf("admin DELETE = %d draining=%t, want 200 and clear", c, s.IsDraining())
	}
}

func TestDrain_TTLValidationAndMethod(t *testing.T) {
	s, ft := drainServer(t)
	for _, body := range []string{`{"ttl_seconds":0}`, `{"ttl_seconds":3601}`, `{"ttl_seconds":-5}`, `{"ttl_seconds":"x"}`, `{`} {
		w := drainCall(t, s, http.MethodPost, drainAdminKey, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "INVALID_TTL") {
			t.Fatalf("body %s = %d %s, want 400 INVALID_TTL", body, w.Code, w.Body.String())
		}
	}
	if s.IsDraining() || ft.count() != 0 {
		t.Fatal("an invalid TTL drained the daemon")
	}
	w := drainCall(t, s, http.MethodPost, drainAdminKey, "")
	if w.Code != http.StatusOK || ft.ds[0] != 300*time.Second {
		t.Fatalf("empty body = %d, ttl %v; want 200 and the 300 s default", w.Code, ft.ds)
	}
	if w := drainCall(t, s, http.MethodGet, drainAdminKey, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", w.Code)
	}
}

func TestDrain_ShutdownMakesItPermanent(t *testing.T) {
	s, ft := drainServer(t)
	if w := drainCall(t, s, http.MethodPost, drainAdminKey, ""); w.Code != http.StatusOK {
		t.Fatalf("POST = %d", w.Code)
	}
	s.SetDraining(true) // SIGTERM: beginDrain
	ft.fire(0)
	if !s.IsDraining() {
		t.Fatal("an expiry cleared the SIGTERM drain")
	}
	if w := drainCall(t, s, http.MethodDelete, drainAdminKey, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "SHUTTING_DOWN") {
		t.Fatalf("DELETE during shutdown = %d %s, want 409 SHUTTING_DOWN", w.Code, w.Body.String())
	}
	w := drainCall(t, s, http.MethodPost, drainAdminKey, "")
	b := decodeDrain(t, w)
	if w.Code != http.StatusOK || !b.Draining || !b.ShuttingDown || b.ExpiresAt != "" {
		t.Fatalf("POST during shutdown = %d %+v, want 200 shutting_down, no deadline", w.Code, b)
	}
	if !s.IsDraining() {
		t.Fatal("the bit was cleared during shutdown")
	}
}

// Review e381 #4, deterministic: both orders of an expiry callback and
// SIGTERM. "SIGTERM first" is a callback that was already running when
// Stop was called and blocked on the mutex: it runs after and must find a
// stale generation. "expiry first" cleared the bit; SIGTERM sets it again.
// Either way the drain ends set and permanent (review ef08 #2).
func TestDrain_ExpiryAndShutdownBothOrders(t *testing.T) {
	for _, expiryFirst := range []bool{false, true} {
		s, ft := drainServer(t)
		if _, err := s.BeginOperatorDrain(time.Minute); err != nil {
			t.Fatal(err)
		}
		if expiryFirst {
			ft.fire(0)
			if s.IsDraining() {
				t.Fatal("the current expiry did not clear the operator drain")
			}
			if got := s.BeginShutdownDrain(); got != 0 {
				t.Fatalf("elapsed after an expiry = %v, want 0 (the full grace)", got)
			}
		} else {
			s.BeginShutdownDrain()
			// fire runs the captured callback unconditionally: Stop on the
			// placeholder timer does not suppress it, so this is a callback
			// already past Stop that now takes the mutex (review e155 #1).
			ft.fire(0)
			if ft.fired() != 1 {
				t.Fatal("the expiry callback did not run after the shutdown drain")
			}
		}
		if !s.IsDraining() {
			t.Fatalf("expiryFirst=%t: the shutdown drain is not set", expiryFirst)
		}
		if err := s.EndOperatorDrain(); err == nil {
			t.Fatalf("expiryFirst=%t: undrain succeeded after SIGTERM", expiryFirst)
		}
	}
}

// The same pair run concurrently, 200 times. It adds coverage only under
// -race (the race detector checks every field access is under the mutex);
// the ordering itself is pinned by TestDrain_ExpiryAndShutdownBothOrders.
func TestDrain_ExpiryRacingShutdownNeverClears(t *testing.T) {
	for i := 0; i < 200; i++ {
		s, ft := drainServer(t)
		if _, err := s.BeginOperatorDrain(time.Minute); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); ft.fire(0) }()
		go func() { defer wg.Done(); s.BeginShutdownDrain() }()
		wg.Wait()
		if !s.IsDraining() {
			t.Fatalf("iteration %d: an expiry racing SIGTERM left the daemon undrained", i)
		}
		if err := s.EndOperatorDrain(); err == nil {
			t.Fatalf("iteration %d: undrain succeeded after SIGTERM", i)
		}
	}
}

// Review e381 #1: the grace decision and the permanent set are one critical
// section. Drained 10 s ago and then undrained: the shutdown sees 0 elapsed,
// so the full grace is slept.
func TestDrain_BeginShutdownDrainReportsElapsedAtomically(t *testing.T) {
	s, _ := drainServer(t)
	base := time.Now()
	now := base
	s.drainNow = func() time.Time { return now }
	if _, err := s.BeginOperatorDrain(time.Minute); err != nil {
		t.Fatal(err)
	}
	now = base.Add(10 * time.Second)
	if err := s.EndOperatorDrain(); err != nil {
		t.Fatal(err)
	}
	if got := s.BeginShutdownDrain(); got != 0 {
		t.Fatalf("elapsed after an undrain = %v, want 0", got)
	}
	s2, _ := drainServer(t)
	s2.drainNow = func() time.Time { return now }
	now = base
	_, _ = s2.BeginOperatorDrain(time.Minute)
	now = base.Add(10 * time.Second)
	if got := s2.BeginShutdownDrain(); got != 10*time.Second {
		t.Fatalf("elapsed under a continuous drain = %v, want 10s", got)
	}
}

func TestDrain_WritesAuditRows(t *testing.T) {
	audit := &drainAuditRepo{}
	s, _ := drainServer(t, WithAdminAuditRepository(audit))
	drainCall(t, s, http.MethodPost, drainAdminKey, `{"ttl_seconds":90}`)
	drainCall(t, s, http.MethodDelete, drainAdminKey, "")
	got := audit.actions()
	if len(got) != 2 || got[0] != "daemon.drain" || got[1] != "daemon.undrain" {
		t.Fatalf("audit actions = %v, want [daemon.drain daemon.undrain]", got)
	}
	audit.mu.Lock()
	after := audit.rows[0].After
	src := audit.rows[0].Source
	audit.mu.Unlock()
	if !strings.Contains(after, `"ttl_seconds":90`) || src != "api" {
		t.Fatalf("drain row after=%q source=%q", after, src)
	}
}

// Review f072 #3: a drain set from an admin browser session is attributed
// to the session user, not to "anonymous-admin".
func TestDrain_AuditAttributesSessionAdmin(t *testing.T) {
	audit := &drainAuditRepo{}
	s, _ := drainServer(t, WithAdminAuditRepository(audit))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/daemon/drain", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, identityKey, &auth.Identity{Extra: map[string]any{
		auth.ExtraSessionRole:   "admin",
		auth.ExtraSessionUserID: "user_42",
	}})
	w := httptest.NewRecorder()
	s.DaemonDrain(w, req.WithContext(ctx))
	if w.Code != http.StatusOK {
		t.Fatalf("session admin POST = %d %s", w.Code, w.Body.String())
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.rows) != 1 || audit.rows[0].Principal != "session:user_42" {
		t.Fatalf("audit rows = %+v, want one with principal session:user_42", audit.rows)
	}
}

// Design review 27d8: a client that hung up after the drain took effect
// still gets its audit row (the insert does not use the request context).
func TestDrain_AuditSurvivesClientDisconnect(t *testing.T) {
	audit := &ctxCheckingAuditRepo{}
	s, _ := drainServer(t, WithAdminAuditRepository(audit))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/daemon/drain", nil)
	ctx, cancel := context.WithCancel(context.WithValue(req.Context(), authEnabledKey, true))
	ctx = context.WithValue(ctx, apiKeyKey, drainAdminKey)
	cancel()
	w := httptest.NewRecorder()
	s.DaemonDrain(w, req.WithContext(ctx))
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body.String())
	}
	if audit.inserted != 1 {
		t.Fatalf("audit rows written with a cancelled request context = %d, want 1", audit.inserted)
	}
}

// ctxCheckingAuditRepo refuses an insert whose context is already done, as
// a database driver would.
type ctxCheckingAuditRepo struct{ inserted int }

func (r *ctxCheckingAuditRepo) Insert(ctx context.Context, _ *persistence.AdminAuditEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.inserted++
	return nil
}

func (r *ctxCheckingAuditRepo) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

// DrainedFor reports how long the bit has been set; beginDrain uses it to
// skip a grace that has already elapsed.
func TestDrain_DrainedForTracksSince(t *testing.T) {
	s, _ := drainServer(t)
	if s.DrainedFor() != 0 {
		t.Fatal("a fresh server reports a drain age")
	}
	base := time.Now()
	s.drainNow = func() time.Time { return base }
	drainCall(t, s, http.MethodPost, drainAdminKey, "")
	s.drainNow = func() time.Time { return base.Add(7 * time.Second) }
	if got := s.DrainedFor(); got != 7*time.Second {
		t.Fatalf("DrainedFor = %v, want 7s", got)
	}
}

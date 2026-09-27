package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// `vornikctl leader-lock release` (issue #60; horizontal scaling LLD,
// implementation contract 2026-09-25). The predicate is the control: the ONE
// DeleteExpired statement decides, the handler only labels a refusal with an
// advisory read afterwards, and the audit comes from the returned row.

// fakeLeaderLocks is an in-memory DaemonLeaderLockRepository. deleteHook runs
// inside DeleteExpired before the predicate is evaluated, which is the seam for
// the renew-between-list-and-delete race.
type fakeLeaderLocks struct {
	rows       map[string]*persistence.DaemonLeaderLock
	deleteHook func(workerID string)
	deleteNows []time.Time
	// afterDelete swaps the row after the statement ran and before the
	// advisory read: a replaced row.
	afterDelete func(workerID string)
}

func (f *fakeLeaderLocks) Acquire(context.Context, string, string, time.Time, time.Duration) (bool, int64, error) {
	return false, 0, nil
}
func (f *fakeLeaderLocks) Renew(context.Context, string, string, time.Time, time.Duration) (bool, error) {
	return false, nil
}
func (f *fakeLeaderLocks) Release(context.Context, string, string) error { return nil }
func (f *fakeLeaderLocks) Get(_ context.Context, id string) (*persistence.DaemonLeaderLock, error) {
	r, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *r
	return &cp, nil
}
func (f *fakeLeaderLocks) List(context.Context) ([]*persistence.DaemonLeaderLock, error) {
	out := make([]*persistence.DaemonLeaderLock, 0, len(f.rows))
	for _, r := range f.rows {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}
func (f *fakeLeaderLocks) DeleteExpired(_ context.Context, id string, now time.Time) (*persistence.DaemonLeaderLock, error) {
	f.deleteNows = append(f.deleteNows, now)
	if f.deleteHook != nil {
		f.deleteHook(id)
	}
	r, ok := f.rows[id]
	if !ok || !persistence.LeaderLockExpired(r.ExpiresAt, now) {
		if f.afterDelete != nil {
			f.afterDelete(id)
		}
		return nil, nil
	}
	delete(f.rows, id)
	return r, nil
}

var llNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func llRow(id string, renewedAgo, expiresIn time.Duration) *persistence.DaemonLeaderLock {
	return &persistence.DaemonLeaderLock{WorkerID: id, HolderID: "host-a:1:" + id, Epoch: 7,
		RenewedAt: llNow.Add(-renewedAgo), ExpiresAt: llNow.Add(expiresIn)}
}

func llServer(locks *fakeLeaderLocks, wired []string, audit *stubAdminAuditRepo, reg prometheus.Registerer) *Server {
	s := &Server{adminAuditRepo: audit}
	WithLeaderLockRelease(locks, func() []string { return wired })(s)
	WithLeaderLockReleaseMetrics(NewLeaderLockReleaseMetrics(reg))(s)
	s.leaderLockNow = func() time.Time { return llNow }
	return s
}

func llPost(t *testing.T, s *Server, body string, authOn bool) (int, leaderLockReleaseResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/leader-locks/release", bytes.NewBufferString(body))
	req = req.WithContext(ContextWithAuthEnabled(req.Context(), authOn))
	if authOn {
		req = req.WithContext(ContextWithAPIKeyForTesting(req.Context(), "sk-admin"))
		s.adminConfig = config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}
		s.adminSurfacePresent = true
	}
	rec := httptest.NewRecorder()
	s.LeaderLockRelease(rec, req)
	var out leaderLockReleaseResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func outcomesOf(r leaderLockReleaseResponse) map[string]string {
	m := map[string]string{}
	for _, x := range r.Results {
		m[x.WorkerID] = x.Outcome
	}
	return m
}

func TestLeaderLockRelease_PerRowOutcomes(t *testing.T) {
	locks := &fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{
		"expired": llRow("expired", 2*time.Hour, -time.Hour),
		"stale":   llRow("stale", 2*leaderLockStaleAfter, time.Minute),
		"active":  llRow("active", time.Second, time.Minute),
	}}
	audit := &stubAdminAuditRepo{}
	reg := prometheus.NewRegistry()
	s := llServer(locks, nil, audit, reg)

	code, out := llPost(t, s, `{"worker_ids":["expired","stale","active","typo"],"reason":"issue 60"}`, false)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	got := outcomesOf(out)
	want := map[string]string{
		"expired": leaderelection.OutcomeReleased, "stale": leaderelection.OutcomeRefusedStale,
		"active": leaderelection.OutcomeRefusedActive, "typo": leaderelection.OutcomeUnknown,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: outcome %q, want %q", id, got[id], w)
		}
	}
	for _, r := range out.Results {
		switch r.Outcome {
		case leaderelection.OutcomeReleased:
			if r.HolderID != "host-a:1:expired" || r.Epoch != 7 || r.Advisory {
				t.Errorf("released row must carry the RETURNING facts and not be advisory: %+v", r)
			}
		default:
			if !r.Advisory {
				t.Errorf("%s: a refusal's label comes from an advisory read and must say so", r.WorkerID)
			}
		}
		if r.Outcome == leaderelection.OutcomeRefusedStale && !strings.Contains(r.Message, "re-run") {
			t.Errorf("refused-stale must say it self-heals: %q", r.Message)
		}
		if r.Outcome == leaderelection.OutcomeRefusedActive && !strings.Contains(r.Message, "restart") {
			t.Errorf("refused-active must give the remediation ladder: %q", r.Message)
		}
	}
	// One audit row per RELEASED row, from the returned row; none for refusals.
	if len(audit.rows) != 1 || audit.rows[0].Action != "leader_lock.release" || audit.rows[0].Target != "expired" ||
		!strings.Contains(audit.rows[0].After, "host-a:1:expired") || !strings.Contains(audit.rows[0].After, "issue 60") {
		t.Fatalf("audit rows: %+v", audit.rows)
	}
	if v := testutil.ToFloat64(s.leaderLockReleaseMetrics.total.WithLabelValues(leaderelection.OutcomeReleased, "named")); v != 1 {
		t.Errorf("released/named counter = %v", v)
	}
	if v := testutil.ToFloat64(s.leaderLockReleaseMetrics.total.WithLabelValues(leaderelection.OutcomeUnknown, "named")); v != 1 {
		t.Errorf("unknown/named counter = %v", v)
	}
}

func TestLeaderLockRelease_TwoReleasesOfOneRowAreReleasedThenUnknown(t *testing.T) {
	locks := &fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{"expired": llRow("expired", 2*time.Hour, -time.Hour)}}
	s := llServer(locks, nil, &stubAdminAuditRepo{}, prometheus.NewRegistry())
	_, first := llPost(t, s, `{"worker_ids":["expired"]}`, false)
	_, second := llPost(t, s, `{"worker_ids":["expired"]}`, false)
	if outcomesOf(first)["expired"] != leaderelection.OutcomeReleased || outcomesOf(second)["expired"] != leaderelection.OutcomeUnknown {
		t.Fatalf("first %+v second %+v", first.Results, second.Results)
	}
	if !strings.Contains(second.Results[0].Message, "another release") {
		t.Errorf("unknown must allow for a lost race: %q", second.Results[0].Message)
	}
}

func TestLeaderLockRelease_AllOrphaned(t *testing.T) {
	locks := &fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{
		"orphan-expired": llRow("orphan-expired", 2*time.Hour, -time.Hour),
		"wired-expired":  llRow("wired-expired", 2*time.Hour, -time.Hour),
		"orphan-active":  llRow("orphan-active", time.Second, time.Minute),
	}}
	audit := &stubAdminAuditRepo{}
	s := llServer(locks, []string{"wired-expired"}, audit, prometheus.NewRegistry())

	if code, _ := llPost(t, s, `{"all_orphaned":true}`, false); code != http.StatusBadRequest {
		t.Fatalf("--all-orphaned without a reason must be refused, got %d", code)
	}
	_, out := llPost(t, s, `{"all_orphaned":true,"reason":"deleted project"}`, false)
	got := outcomesOf(out)
	if len(got) != 1 || got["orphan-expired"] != leaderelection.OutcomeReleased {
		t.Fatalf("only not-wired expired rows are targeted: %+v", out.Results)
	}
	if _, ok := locks.rows["wired-expired"]; !ok {
		t.Fatal("a wired expired row must not be taken by --all-orphaned")
	}
	if len(audit.rows) != 1 || !strings.Contains(audit.rows[0].After, `"bulk":true`) {
		t.Fatalf("bulk audit: %+v", audit.rows)
	}

	_, none := llPost(t, s, `{"all_orphaned":true,"reason":"again"}`, false)
	if len(none.Results) != 0 {
		t.Fatalf("nothing left to release: %+v", none.Results)
	}
}

// The race the single statement closes, on the --all-orphaned path (the only
// one that classifies before deleting): the holder renews between the list and
// the delete, so the statement refuses and the row reports refused-active.
func TestLeaderLockRelease_RenewBetweenListAndDeleteIsRefused(t *testing.T) {
	locks := &fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{"orphan": llRow("orphan", 2*time.Hour, -time.Hour)}}
	locks.deleteHook = func(id string) {
		locks.rows[id].RenewedAt = llNow
		locks.rows[id].ExpiresAt = llNow.Add(time.Minute)
	}
	s := llServer(locks, nil, &stubAdminAuditRepo{}, prometheus.NewRegistry())
	_, out := llPost(t, s, `{"all_orphaned":true,"reason":"race"}`, false)
	if outcomesOf(out)["orphan"] != leaderelection.OutcomeRefusedActive {
		t.Fatalf("a renewed row must be refused, not released: %+v", out.Results)
	}
	if _, ok := locks.rows["orphan"]; !ok {
		t.Fatal("the renewed row must survive")
	}
}

// now is captured once per request: every DeleteExpired sees the same instant,
// so rows that straddle an expiry get outcomes consistent with one clock.
func TestLeaderLockRelease_OneNowPerRequest(t *testing.T) {
	locks := &fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{
		"a": llRow("a", 2*time.Hour, -time.Hour),
		"b": llRow("b", 2*time.Hour, -time.Hour),
	}}
	s := llServer(locks, nil, &stubAdminAuditRepo{}, prometheus.NewRegistry())
	calls := 0
	s.leaderLockNow = func() time.Time { calls++; return llNow.Add(time.Duration(calls) * time.Hour) }
	llPost(t, s, `{"worker_ids":["a","b"]}`, false)
	if calls != 1 {
		t.Fatalf("now read %d times per request, want 1", calls)
	}
	if len(locks.deleteNows) != 2 || !locks.deleteNows[0].Equal(locks.deleteNows[1]) {
		t.Fatalf("each DeleteExpired must get the same now: %v", locks.deleteNows)
	}
}

func TestLeaderLockRelease_Usage(t *testing.T) {
	s := llServer(&fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{}}, nil, &stubAdminAuditRepo{}, prometheus.NewRegistry())
	for _, body := range []string{`{}`, `{"worker_ids":[],"all_orphaned":false}`, `{"worker_ids":["x"],"all_orphaned":true,"reason":"r"}`, `not json`} {
		if code, _ := llPost(t, s, body, false); code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", body, code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/leader-locks/release", nil)
	rec := httptest.NewRecorder()
	s.LeaderLockRelease(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: want 405, got %d", rec.Code)
	}
}

// Worker ids embed project ids and the table has no tenant boundary yet, so the
// known-id listing on `unknown` is shown only to an authenticated admin.
func TestLeaderLockRelease_UnknownListingElidedWhenAuthOff(t *testing.T) {
	locks := &fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{"email_imap_receiver_acme": llRow("email_imap_receiver_acme", time.Second, time.Minute)}}
	s := llServer(locks, nil, &stubAdminAuditRepo{}, prometheus.NewRegistry())
	_, off := llPost(t, s, `{"worker_ids":["typo"]}`, false)
	if len(off.KnownWorkerIDs) != 0 {
		t.Fatalf("auth off: the listing must be elided, got %v", off.KnownWorkerIDs)
	}
	_, on := llPost(t, s, `{"worker_ids":["typo"]}`, true)
	if len(on.KnownWorkerIDs) != 1 || on.KnownWorkerIDs[0] != "email_imap_receiver_acme" {
		t.Fatalf("auth on, admin caller: the known ids are listed, got %v", on.KnownWorkerIDs)
	}
}

// The doctor and the statement share one expiry definition: at
// expires_at == now the row is not expired for either.
func TestLeaderLockRelease_DoctorAgreesAtTheBoundary(t *testing.T) {
	r := &persistence.DaemonLeaderLock{HolderID: "h", RenewedAt: llNow.Add(-2 * leaderLockStaleAfter), ExpiresAt: llNow}
	if state, _ := classifyLeaderLock(r, llNow); state == "EXPIRED" {
		t.Fatal("at expires_at == now the doctor must not call the row EXPIRED: the release would refuse it")
	}
}

// refused-changed: the statement refused a live row, and by the advisory read a
// DIFFERENT, expired row is there. Classified against the same now, so a clock
// tick cannot produce it.
func TestLeaderLockRelease_ReplacedRowIsRefusedChanged(t *testing.T) {
	locks := &fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{"w": llRow("w", time.Second, time.Minute)}}
	locks.afterDelete = func(id string) { locks.rows[id] = llRow(id, 2*time.Hour, -time.Hour) }
	s := llServer(locks, nil, &stubAdminAuditRepo{}, prometheus.NewRegistry())
	_, out := llPost(t, s, `{"worker_ids":["w"]}`, false)
	if outcomesOf(out)["w"] != leaderelection.OutcomeRefusedChanged || !strings.Contains(out.Results[0].Message, "re-run") {
		t.Fatalf("got %+v", out.Results)
	}
}

type failingLLAudit struct{ stubAdminAuditRepo }

func (*failingLLAudit) Insert(context.Context, *persistence.AdminAuditEntry) error {
	return context.DeadlineExceeded
}

// A failed audit write does not undo a release: the row is gone by then (the
// audit is written from the returned row). It is reported as audit_error, and
// the CLI exits 1 for it (review-20260925-1f8d T1).
func TestLeaderLockRelease_FailedAuditIsReportedNotHidden(t *testing.T) {
	locks := &fakeLeaderLocks{rows: map[string]*persistence.DaemonLeaderLock{"expired": llRow("expired", 2*time.Hour, -time.Hour)}}
	s := &Server{adminAuditRepo: &failingLLAudit{}}
	WithLeaderLockRelease(locks, nil)(s)
	s.leaderLockNow = func() time.Time { return llNow }
	_, out := llPost(t, s, `{"worker_ids":["expired"]}`, false)
	if len(out.Results) != 1 || out.Results[0].Outcome != leaderelection.OutcomeReleased || out.Results[0].AuditError == "" {
		t.Fatalf("want released with audit_error, got %+v", out.Results)
	}
	if _, still := locks.rows["expired"]; still {
		t.Fatal("the row is released whatever the audit did")
	}
}

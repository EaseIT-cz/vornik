package approverdevice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

type pushed struct{ subject, body string }

type fixture struct {
	svc    *Service
	repo   persistence.ApproverDeviceRepository
	now    time.Time
	mu     sync.Mutex
	pushes []pushed
}

func (f *fixture) clock() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func newFixture(t *testing.T, opts ...Option) *fixture {
	t.Helper()
	db := sqlitetest.File(t, "ad.db")
	f := &fixture{repo: sqlite.NewApproverDeviceRepository(db.DB), now: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
	base := []Option{
		WithClock(f.clock),
		WithOrigin("https://vornik.example/"),
		WithNotifier(func(_ context.Context, subject, body string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.pushes = append(f.pushes, pushed{subject, body})
		}),
	}
	f.svc = New(f.repo, append(base, opts...)...)
	return f
}

func (f *fixture) pair(t *testing.T, label string) string {
	t.Helper()
	code, exp, err := f.svc.StartPairing(context.Background(), label)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(f.clock().Add(PairingTTL)) {
		t.Fatalf("pairing expires %v, want %v", exp, f.clock().Add(PairingTTL))
	}
	return code
}

func (f *fixture) firstDevice(t *testing.T) (*Device, string) {
	t.Helper()
	r, err := f.svc.Redeem(context.Background(), f.pair(t, "Pixel"), "10.0.0.1")
	if err != nil || r.DeviceToken == "" || r.Device == nil {
		t.Fatalf("first redeem = %+v, %v", r, err)
	}
	return r.Device, r.DeviceToken
}

// assertNoSecretsPushed: the push channel may be a mail workflow the agent
// defined, so a push never carries a code or a token (design §9.3).
func (f *fixture) countPushesContaining(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.pushes {
		if strings.Contains(p.subject+" "+p.body, sub) {
			n++
		}
	}
	return n
}

func (f *fixture) assertNoPushContaining(t *testing.T, sub string) {
	t.Helper()
	if n := f.countPushesContaining(sub); n != 0 {
		t.Fatalf("%d push(es) contain %q, want none", n, sub)
	}
}

func (f *fixture) assertNoSecretsPushed(t *testing.T, secrets ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pushes {
		for _, s := range secrets {
			if s != "" && (strings.Contains(p.body, s) || strings.Contains(p.subject, s)) {
				t.Fatalf("a push carried a secret: %+v", p)
			}
		}
	}
}

func TestFirstDevice_PairsAndAuthenticates(t *testing.T) {
	f := newFixture(t)
	code := f.pair(t, "Pixel")
	r, err := f.svc.Redeem(context.Background(), code, "10.0.0.1")
	if err != nil || r.DeviceToken == "" || r.ClaimToken != "" {
		t.Fatalf("redeem = %+v, %v", r, err)
	}
	d, err := f.svc.Authenticate(context.Background(), r.DeviceToken)
	if err != nil || d.Label != "Pixel" {
		t.Fatalf("authenticate = %+v, %v", d, err)
	}
	if len(f.pushes) != 1 || !strings.Contains(f.pushes[0].body, `"Pixel"`) || !strings.Contains(f.pushes[0].body, "https://vornik.example/ui/approve/devices") {
		t.Fatalf("enrollment push = %+v", f.pushes)
	}
	f.assertNoSecretsPushed(t, code, r.DeviceToken)
	if _, err := f.svc.Redeem(context.Background(), code, "10.0.0.1"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("reused code: %v", err)
	}
}

// The design §9.2 rule: once a device exists, a pairing code alone is not
// enough. The new device holds only a claim until an existing device
// approves, and no device token exists before that.
func TestFurtherDevice_NeedsApprovalFromAnExistingDevice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, _ := f.firstDevice(t)
	code := f.pair(t, "Tablet")
	r, err := f.svc.Redeem(ctx, code, "10.0.0.2")
	if err != nil || r.DeviceToken != "" || r.ClaimToken == "" {
		t.Fatalf("second redeem = %+v, %v", r, err)
	}
	if tok, st, err := f.svc.PollClaim(ctx, r.ClaimToken); err != nil || st != ClaimPending || tok != "" {
		t.Fatalf("poll before approval = %q %s %v", tok, st, err)
	}
	pend, _ := f.svc.ListPending(ctx)
	if len(pend) != 1 || !strings.Contains(pend[0].Sentence, `"Tablet"`) {
		t.Fatalf("pending = %+v", pend)
	}
	if err := f.svc.Decide(ctx, first, pend[0].ID, pend[0].RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	tok, st, err := f.svc.PollClaim(ctx, r.ClaimToken)
	if err != nil || st != ClaimApproved || tok == "" {
		t.Fatalf("poll after approval = %q %s %v", tok, st, err)
	}
	// T11: "used" is last_used_at moving off paired_at, so the clock must move.
	f.advance(time.Second)
	d, err := f.svc.Authenticate(ctx, tok)
	if err != nil || d.Label != "Tablet" {
		t.Fatalf("new device = %+v, %v", d, err)
	}
	if _, st, _ := f.svc.PollClaim(ctx, r.ClaimToken); st != ClaimExpired {
		t.Fatalf("a completed claim polled again = %s, want expired (a used device is not re-minted)", st)
	}
	devs, _ := f.svc.ListDevices(ctx)
	var tablet *persistence.ApproverDeviceRow
	for i := range devs {
		if devs[i].Label == "Tablet" {
			tablet = &devs[i]
		}
	}
	if len(devs) != 2 || tablet == nil || tablet.PairedBy != "device:"+first.ID {
		t.Fatalf("devices = %+v", devs)
	}
	f.assertNoSecretsPushed(t, code, r.ClaimToken, tok)
}

func TestFurtherDevice_RejectedAndExpired(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, _ := f.firstDevice(t)
	r, _ := f.svc.Redeem(ctx, f.pair(t, "Unknown"), "10.0.0.3")
	pend, _ := f.svc.ListPending(ctx)
	if err := f.svc.Decide(ctx, first, pend[0].ID, pend[0].RenderedSHA256, false); err != nil {
		t.Fatal(err)
	}
	if tok, st, _ := f.svc.PollClaim(ctx, r.ClaimToken); st != ClaimRejected || tok != "" {
		t.Fatalf("rejected = %q %s", tok, st)
	}
	r2, _ := f.svc.Redeem(ctx, f.pair(t, "Late"), "10.0.0.3")
	f.advance(ClaimTTL + time.Second)
	if tok, st, _ := f.svc.PollClaim(ctx, r2.ClaimToken); st != ClaimExpired || tok != "" {
		t.Fatalf("expired = %q %s", tok, st)
	}
	if _, st, _ := f.svc.PollClaim(ctx, "never-issued"); st != ClaimExpired {
		t.Fatalf("unknown claim = %s", st)
	}
}

func TestRedeem_LimitsAttemptsWithoutConsumingTheCode(t *testing.T) {
	f := newFixture(t, WithLimiters(chatauth.NewRedemptionLimiterWith(3, time.Hour), chatauth.NewRedemptionLimiterWith(100, time.Hour)))
	ctx := context.Background()
	code := f.pair(t, "Pixel")
	for i := 0; i < 3; i++ {
		if _, err := f.svc.Redeem(ctx, "WRONG-CODE", "10.9.9.9"); !errors.Is(err, ErrBadCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := f.svc.Redeem(ctx, code, "10.9.9.9"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("4th attempt from the same IP: %v, want ErrRateLimited", err)
	}
	if r, err := f.svc.Redeem(ctx, code, "10.1.1.1"); err != nil || r.DeviceToken == "" {
		t.Fatalf("the limited attempt consumed the code: %+v, %v", r, err)
	}
}

func TestRedeem_GlobalLimit(t *testing.T) {
	f := newFixture(t, WithLimiters(chatauth.NewRedemptionLimiterWith(100, time.Hour), chatauth.NewRedemptionLimiterWith(2, time.Hour)))
	for i, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		if _, err := f.svc.Redeem(context.Background(), "WRONG", ip); !errors.Is(err, ErrBadCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := f.svc.Redeem(context.Background(), "WRONG", "10.0.0.3"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a fresh IP past the global budget: %v", err)
	}
}

func TestAuthenticate_RevokedIdleAndTouch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, tok := f.firstDevice(t)
	if _, err := f.svc.Authenticate(ctx, ""); !errors.Is(err, ErrNoDevice) {
		t.Fatal("empty token authenticated")
	}
	if _, err := f.svc.Authenticate(ctx, "forged"); !errors.Is(err, ErrNoDevice) {
		t.Fatal("unknown token authenticated")
	}
	f.advance(2 * time.Hour)
	if _, err := f.svc.Authenticate(ctx, tok); err != nil {
		t.Fatal(err)
	}
	f.advance(IdleExpiry - time.Hour) // within 90 days of the touch above
	if _, err := f.svc.Authenticate(ctx, tok); err != nil {
		t.Fatalf("used within 90 days, refused: %v", err)
	}
	f.advance(IdleExpiry + time.Minute)
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, ErrNoDevice) {
		t.Fatal("a device idle past 90 days authenticated")
	}
	f2 := newFixture(t)
	d2, tok2 := f2.firstDevice(t)
	if err := f2.svc.Revoke(ctx, d2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.svc.Authenticate(ctx, tok2); !errors.Is(err, ErrNoDevice) {
		t.Fatal("a revoked device authenticated")
	}
	_ = d
}

// Design §9.2, amendment 2026-10-05 (P1, devices unpaired by a lost rotation
// response): rotations from one value return ONE successor; the old value
// stays valid until the successor is presented, then dies; a value that is
// neither current nor previous is stale.
func TestRotate_SuccessorIsIdempotentAndConfirmedOnUse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, tok := f.firstDevice(t)
	s1, err := f.svc.Rotate(ctx, d, tok)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := f.svc.Rotate(ctx, d, tok) // the lost response's retry, or the other tab
	if err != nil || s2 != s1 {
		t.Fatalf("second rotation from the same value: %q, %v; want the same successor", s2, err)
	}
	if _, err := f.svc.Authenticate(ctx, tok); err != nil {
		t.Fatalf("the old value before the successor was presented: %v", err)
	}
	if _, err := f.svc.Authenticate(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(ctx, tok); !errors.Is(err, ErrNoDevice) {
		t.Fatal("the old value still authenticates after the successor was presented")
	}
	if _, err := f.svc.Rotate(ctx, d, tok); !errors.Is(err, ErrStaleDevice) {
		t.Fatalf("rotating from a confirmed-away value: %v", err)
	}
	// A fresh rotation from the confirmed value draws a fresh nonce.
	s3, err := f.svc.Rotate(ctx, d, s1)
	if err != nil || s3 == s1 || s3 == "" {
		t.Fatalf("next rotation: %q, %v", s3, err)
	}
	// Revocation ends both values.
	if err := f.svc.Revoke(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{s1, s3} {
		if _, err := f.svc.Authenticate(ctx, v); !errors.Is(err, ErrNoDevice) {
			t.Fatalf("a revoked device's value authenticates: %v", err)
		}
	}
}

// The successor is computed from the PRESENTED plaintext and the nonce: the
// stored row (hashes and nonce) alone does not yield it, and a settled row
// keeps no nonce at all.
func TestRotate_SuccessorNeedsThePresentedPlaintext(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, tok := f.firstDevice(t)
	s1, err := f.svc.Rotate(ctx, d, tok)
	if err != nil {
		t.Fatal(err)
	}
	row, err := f.repo.GetDeviceByTokenHash(ctx, HashToken(s1))
	if err != nil {
		t.Fatal(err)
	}
	if successor(d.ID, row.PrevTokenHash, row.RotationNonce) == s1 {
		t.Fatal("the successor is derivable from the stored hash and nonce")
	}
	if successor(d.ID, tok, row.RotationNonce) != s1 {
		t.Fatal("the successor is not succ(id, presented, nonce)")
	}
	if _, err := f.svc.Authenticate(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if row, _ = f.repo.GetDeviceByTokenHash(ctx, HashToken(s1)); row.RotationNonce != "" {
		t.Fatal("the nonce survived confirmation")
	}
}

func TestDecide_RefusalsComeBeforeTheTransition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	req := persistence.AgentApprovalRequestRow{ID: "apr_p3", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}
	if err := f.repo.CreateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, nil, "apr_p3", "h", true); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("nil device: %v", err)
	}
	if err := f.svc.Decide(ctx, d, "apr_p3", "h", true); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("a kind with no effect: %v", err)
	}
	if got, _ := f.repo.GetRequest(ctx, "apr_p3"); got.Status != persistence.ApprovalPending {
		t.Fatalf("the refused decision moved the row to %s", got.Status)
	}
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error { return nil })
	if err := f.svc.Decide(ctx, d, "apr_p3", "changed-after-display", true); !errors.Is(err, ErrNotDecidable) {
		t.Fatalf("hash mismatch: %v", err)
	}
	if err := f.svc.Decide(ctx, d, "apr_missing", "h", true); !errors.Is(err, ErrNotDecidable) {
		t.Fatalf("unknown request: %v", err)
	}
}

// An effect that fails leaves the request approved-unapplied; Tick re-applies
// it, so every effect runs at least once and must tolerate running twice.
func TestEffects_ReappliedUntilTheyStick(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	var calls int
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	})
	if err := f.repo.CreateRequest(ctx, persistence.AgentApprovalRequestRow{ID: "apr_w", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_w", "h", true); err == nil || !strings.Contains(err.Error(), "retried") {
		t.Fatalf("a failing effect: %v", err)
	}
	if u, _ := f.repo.ListApprovedUnapplied(ctx); len(u) != 1 {
		t.Fatalf("approved-unapplied = %d, want 1", len(u))
	}
	if err := f.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.repo.ListApprovedUnapplied(ctx); len(u) != 0 || calls != 2 {
		t.Fatalf("after tick: unapplied=%d calls=%d", len(u), calls)
	}
	if err := f.svc.Tick(ctx); err != nil || calls != 2 {
		t.Fatalf("an applied effect ran again: calls=%d %v", calls, err)
	}
}

// The device_enrollment effect is registered and a no-op (plan amendment 3);
// running it twice changes nothing.
func TestEnrollmentEffect_IsIdempotentNoOp(t *testing.T) {
	f := newFixture(t)
	fn, ok := f.svc.effect(persistence.ApprovalKindDeviceEnrollment)
	if !ok {
		t.Fatal("device_enrollment has no effect; its requests could never be approved")
	}
	for i := 0; i < 2; i++ {
		if err := fn(context.Background(), persistence.AgentApprovalRequestRow{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTick_ExpiresPendingRequests(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.firstDevice(t)
	_, _ = f.svc.Redeem(ctx, f.pair(t, "Slow"), "10.0.0.4")
	f.advance(ClaimTTL + time.Minute)
	if err := f.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.svc.ListPending(ctx); len(p) != 0 {
		t.Fatalf("pending after expiry tick = %d", len(p))
	}
}

func TestCleanLabel(t *testing.T) {
	for in, want := range map[string]string{"Pixel": "Pixel", "  Pix\x00el\n ": "Pixel", "Žluťoučký kůň": "Žluťoučký kůň"} {
		if got, err := CleanLabel(in); err != nil || got != want {
			t.Errorf("CleanLabel(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "   ", "\x01\x02", strings.Repeat("x", 41)} {
		if _, err := CleanLabel(bad); !errors.Is(err, ErrBadLabel) {
			t.Errorf("CleanLabel(%q) accepted", bad)
		}
	}
}

func TestNoNotifier_PushConfiguredFalse(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if !f.svc.PushConfigured() {
		t.Fatal("notifier set but PushConfigured false")
	}
	s := New(f.repo)
	if s.PushConfigured() {
		t.Fatal("no notifier but PushConfigured true")
	}
	code, _, _ := s.StartPairing(ctx, "Quiet")
	if _, err := s.Redeem(ctx, code, "10.0.0.5"); err != nil {
		t.Fatalf("pairing without a push channel: %v", err)
	}
}

// Review 20261002-4de8 F1: the synchronous path hands the effect the row as
// decided (approver, decision time), exactly as the re-apply loop does.
func TestDecide_EffectSeesTheDecidedRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	var seen persistence.AgentApprovalRequestRow
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(_ context.Context, r persistence.AgentApprovalRequestRow) error {
		seen = r
		return nil
	})
	if err := f.repo.CreateRequest(ctx, persistence.AgentApprovalRequestRow{ID: "apr_d", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_d", "h", true); err != nil {
		t.Fatal(err)
	}
	if seen.Status != persistence.ApprovalApproved || seen.DecidedByDevice != d.ID || seen.DecidedAt == nil {
		t.Fatalf("the effect saw %+v, want the approved row with its approver", seen)
	}
}

// Review 20261002-4de8 F2: two services (two cluster nodes) ticking over the
// same approved request run its effect once.
func TestReapply_OneNodeAtATime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	other := New(f.repo, WithClock(f.clock))
	var mu sync.Mutex
	calls := 0
	block := make(chan struct{})
	run := func() {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			<-block // the first node is mid-effect while the second ticks
		}
	}
	effect := Effect(func(context.Context, persistence.AgentApprovalRequestRow) error { run(); return nil })
	failOnce := true
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(c context.Context, r persistence.AgentApprovalRequestRow) error {
		if failOnce {
			failOnce = false
			return errors.New("transient")
		}
		return effect(c, r)
	})
	other.RegisterEffect(persistence.ApprovalKindWideningChange, effect)
	if err := f.repo.CreateRequest(ctx, persistence.AgentApprovalRequestRow{ID: "apr_n", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_n", "h", true); err == nil || !strings.Contains(err.Error(), "apr_n") {
		t.Fatalf("the failure does not name the request: %v", err)
	}
	f.advance(applyLease + time.Second) // the failed attempt's lease lapses
	done := make(chan error, 1)
	go func() { done <- f.svc.Tick(ctx) }()
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := other.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("the effect ran %d times across two nodes, want 1", calls)
	}
}

// Plan P3.6: an effect that can never succeed (ErrPermanent) ends the
// request with its reason instead of being retried every minute forever.
func TestEffects_PermanentFailureIsRecordedNotRetried(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	calls := 0
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error {
		calls++
		return fmt.Errorf("%w: the file changed since this was prepared", ErrPermanent)
	})
	if err := f.repo.CreateRequest(ctx, persistence.AgentApprovalRequestRow{ID: "apr_perm", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_perm", "h", true); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("the permanent failure was not reported: %v", err)
	}
	r, _ := f.repo.GetRequest(ctx, "apr_perm")
	if r.AppliedAt == nil || !strings.Contains(r.ApplyError, "changed since") {
		t.Fatalf("the failure was not recorded: %+v", r)
	}
	f.advance(applyLease + time.Second)
	if err := f.svc.Tick(ctx); err != nil || calls != 1 {
		t.Fatalf("a permanent failure was retried: calls=%d %v", calls, err)
	}
}

func TestApproverDevice_TerminalObserverSeesApproveRejectFailureAndExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error { return nil })
	f.svc.RegisterEffect(persistence.ApprovalKindCredentialSlot, func(context.Context, persistence.AgentApprovalRequestRow) error {
		return fmt.Errorf("%w: deliberately permanent", ErrPermanent)
	})
	var got []string
	f.svc.RegisterTerminalObserver(persistence.ApprovalKindWideningChange, func(_ context.Context, r persistence.AgentApprovalRequestRow, status string) {
		got = append(got, r.ID+":"+r.Namespace+":"+r.Kind+":"+status)
	})
	f.svc.RegisterTerminalObserver(persistence.ApprovalKindCredentialSlot, func(_ context.Context, r persistence.AgentApprovalRequestRow, status string) {
		got = append(got, r.ID+":"+r.Namespace+":"+r.Kind+":"+status)
	})
	rows := []persistence.AgentApprovalRequestRow{
		{ID: "apr_ok", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange, Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h1", Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)},
		{ID: "apr_rej", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange, Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h2", Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)},
		{ID: "apr_fail", Namespace: "hermes", Kind: persistence.ApprovalKindCredentialSlot, Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h3", Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)},
		{ID: "apr_exp", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange, Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h4", Status: persistence.ApprovalPending, CreatedAt: f.clock().Add(-2 * RequestTTL), ExpiresAt: f.clock().Add(-RequestTTL)},
	}
	for _, r := range rows {
		if err := f.repo.CreateRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.Decide(ctx, d, "apr_ok", "h1", true); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_rej", "h2", false); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_fail", "h3", true); err == nil || !strings.Contains(err.Error(), "deliberately permanent") {
		t.Fatalf("permanent failure = %v", err)
	}
	if err := f.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"apr_ok:hermes:widening_change:approved",
		"apr_rej:hermes:widening_change:rejected",
		"apr_fail:hermes:credential_slot:failed",
		"apr_exp:hermes:widening_change:expired",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestApproverDevice_TerminalObserverPanicsAreContained(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error { return nil })
	f.svc.RegisterEffect(persistence.ApprovalKindCredentialSlot, func(context.Context, persistence.AgentApprovalRequestRow) error {
		return fmt.Errorf("%w: deliberately permanent", ErrPermanent)
	})
	var got []string
	panicObserver := func(context.Context, persistence.AgentApprovalRequestRow, string) { panic("observer boom") }
	record := func(_ context.Context, r persistence.AgentApprovalRequestRow, status string) {
		got = append(got, r.ID+":"+status)
	}
	f.svc.RegisterTerminalObserver(persistence.ApprovalKindWideningChange, panicObserver)
	f.svc.RegisterTerminalObserver(persistence.ApprovalKindWideningChange, record)
	f.svc.RegisterTerminalObserver(persistence.ApprovalKindCredentialSlot, panicObserver)
	f.svc.RegisterTerminalObserver(persistence.ApprovalKindCredentialSlot, record)
	rows := []persistence.AgentApprovalRequestRow{
		{ID: "apr_ok_panic", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange, Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h1", Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)},
		{ID: "apr_rej_panic", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange, Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h2", Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)},
		{ID: "apr_fail_panic", Namespace: "hermes", Kind: persistence.ApprovalKindCredentialSlot, Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h3", Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)},
		{ID: "apr_exp_panic", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange, Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h4", Status: persistence.ApprovalPending, CreatedAt: f.clock().Add(-2 * RequestTTL), ExpiresAt: f.clock().Add(-RequestTTL)},
	}
	for _, r := range rows {
		if err := f.repo.CreateRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.Decide(ctx, d, "apr_ok_panic", "h1", true); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_rej_panic", "h2", false); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_fail_panic", "h3", true); err == nil || !strings.Contains(err.Error(), "deliberately permanent") {
		t.Fatalf("permanent failure = %v", err)
	}
	if err := f.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"apr_ok_panic:approved", "apr_rej_panic:rejected", "apr_fail_panic:failed", "apr_exp_panic:expired"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Plan P3.6: a rejection runs the kind's reject hook (the proposal behind a
// widening change is rejected with it).
func TestDecide_RejectRunsTheHook(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	var rejected string
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error { return nil })
	f.svc.RegisterOnReject(persistence.ApprovalKindWideningChange, func(_ context.Context, r persistence.AgentApprovalRequestRow) {
		rejected = r.ID
	})
	if err := f.repo.CreateRequest(ctx, persistence.AgentApprovalRequestRow{ID: "apr_rej", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_rej", "h", false); err != nil {
		t.Fatal(err)
	}
	if rejected != "apr_rej" {
		t.Fatal("the reject hook did not run")
	}
}

// FileRequest stores a request and pushes its sentence and link, never more.
func TestFileRequest_PushesSentenceAndLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	row := persistence.AgentApprovalRequestRow{ID: "apr_file", Namespace: "hermes", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "Your assistant wants X.", Rendered: []byte(`{"secretish":"no"}`), RenderedSHA256: "h",
		Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}
	if err := f.svc.FileRequest(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.GetRequest(ctx, "apr_file"); err != nil {
		t.Fatal(err)
	}
	last := f.pushes[len(f.pushes)-1]
	if !strings.Contains(last.body, "Your assistant wants X.") || !strings.Contains(last.body, "https://vornik.example/ui/approve/apr_file") || strings.Contains(last.body, "secretish") {
		t.Fatalf("push = %+v", last)
	}
}

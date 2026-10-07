package repotest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunApproverDeviceSuite pins approver devices, pairings and approval requests
// on both drivers (agent-administered Vornik design §9; plan P2.1).
//
// repo is the repository on the driver's real, migrated schema; the miss
// contract, which no stored row can disturb, runs there. Every other subtest
// runs on a FRESH store from fresh(t): whether a redemption is the first
// device depends on the global device set, which the shared, never-truncated
// integration database cannot offer empty, so the Postgres lane hands out a
// new schema per call and SQLite a new database file.
func RunApproverDeviceSuite(t *testing.T, repo persistence.ApproverDeviceRepository, fresh func(t *testing.T) persistence.ApproverDeviceRepository) {
	t.Helper()
	t.Run("MissContract", func(t *testing.T) {
		approverMissContract(t, &approverHarness{repo: repo, ctx: context.Background(), now: time.Now().UTC()})
	})
	t.Run("First_redeem_creates_the_device", func(t *testing.T) { approverFirstRedeem(t, newApproverHarness(t, fresh)) })
	t.Run("Later_redeem_files_an_enrollment_request", func(t *testing.T) { approverLaterRedeem(t, newApproverHarness(t, fresh)) })
	t.Run("Unknown_expired_and_used_codes_are_one_miss", func(t *testing.T) { approverRedeemRefusals(t, newApproverHarness(t, fresh)) })
	t.Run("ConcurrentFirstRedeem", func(t *testing.T) { approverConcurrentFirstRedeem(t, newApproverHarness(t, fresh)) })
	t.Run("CompletePairing_runs_once", func(t *testing.T) { approverCompleteOnce(t, newApproverHarness(t, fresh)) })
	t.Run("Remint_enrollment_token_guards", func(t *testing.T) { approverRemint(t, newApproverHarness(t, fresh)) })
	t.Run("Rotate_touch_revoke", func(t *testing.T) { approverRotateTouchRevoke(t, newApproverHarness(t, fresh)) })
	t.Run("Share_close_expire_and_streak", func(t *testing.T) { approverShareLifecycle(t, newApproverHarness(t, fresh)) })
	t.Run("Resume_only_an_expired_value_with_a_code", func(t *testing.T) { approverResume(t, newApproverHarness(t, fresh)) })
	t.Run("Decide_is_hash_bound_and_once", func(t *testing.T) { approverDecide(t, newApproverHarness(t, fresh)) })
	t.Run("Applied_and_expiry", func(t *testing.T) { approverAppliedAndExpiry(t, newApproverHarness(t, fresh)) })
	t.Run("ClaimApply_is_one_holder_at_a_time", func(t *testing.T) { approverClaimApply(t, newApproverHarness(t, fresh)) })
	t.Run("Broker_action_requests_store", func(t *testing.T) { approverBrokerActionKind(t, newApproverHarness(t, fresh)) })
	// Hermes approval transport design §4.1, §4.2, §4.4.
	t.Run("Host_action_decision_records_its_choice", func(t *testing.T) { approverHostActionChoice(t, newApproverHarness(t, fresh)) })
	t.Run("CreateRequestCapped_bounds_pending_and_recent", func(t *testing.T) { approverCappedCreate(t, newApproverHarness(t, fresh)) })
	t.Run("CreateRequestCapped_two_concurrent_at_the_limit", func(t *testing.T) { approverCappedCreateConcurrent(t, newApproverHarness(t, fresh)) })
	t.Run("ExpirePendingRows_returns_what_it_expired", func(t *testing.T) { approverExpirePendingRows(t, newApproverHarness(t, fresh)) })
}

// approverBrokerActionKind: plan P4.8's broker_action kind is accepted on
// both drivers (the kind is CHECK-constrained).
func approverBrokerActionKind(t *testing.T, h *approverHarness) {
	r := persistence.AgentApprovalRequestRow{ID: "apr_ba_1", Namespace: "hermes", Kind: persistence.ApprovalKindBrokerAction,
		Sentence: "s", Rendered: []byte(`{"action_id":"ba_1"}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: h.now, ExpiresAt: h.now.Add(time.Hour)}
	if err := h.repo.CreateRequest(h.ctx, r); err != nil {
		t.Fatalf("a broker_action request was refused: %v", err)
	}
	got, err := h.repo.GetRequest(h.ctx, "apr_ba_1")
	if err != nil || got.Kind != persistence.ApprovalKindBrokerAction {
		t.Fatalf("read back %+v %v", got, err)
	}
}

type approverHarness struct {
	repo persistence.ApproverDeviceRepository
	ctx  context.Context
	now  time.Time
}

func newApproverHarness(t *testing.T, fresh func(t *testing.T) persistence.ApproverDeviceRepository) *approverHarness {
	return &approverHarness{repo: fresh(t), ctx: context.Background(), now: time.Now().UTC().Truncate(time.Second)}
}

func (h *approverHarness) pairing(t *testing.T, code string) {
	t.Helper()
	if err := h.repo.CreatePairing(h.ctx, persistence.ApproverPairingRow{
		CodeHash: "code-" + code, Label: "Phone " + code, CreatedAt: h.now, ExpiresAt: h.now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *approverHarness) device(id string) persistence.ApproverDeviceRow {
	return persistence.ApproverDeviceRow{ID: id, Label: "Phone " + id, TokenHash: "tok-" + id,
		PairedAt: h.now, PairedBy: "first-device", LastUsedAt: h.now}
}

func (h *approverHarness) request(id string) persistence.AgentApprovalRequestRow {
	return persistence.AgentApprovalRequestRow{ID: id, Kind: persistence.ApprovalKindDeviceEnrollment,
		Sentence: "A new device wants to approve for you.", Rendered: []byte(`{"kind":"device_enrollment","label":"x"}`),
		RenderedSHA256: "sha-" + id, Status: persistence.ApprovalPending, CreatedAt: h.now, ExpiresAt: h.now.Add(7 * 24 * time.Hour)}
}

func (h *approverHarness) redeem(t *testing.T, code string) bool {
	t.Helper()
	first, err := h.repo.RedeemPairing(h.ctx, "code-"+code, "claim-"+code, h.device("dev_"+code), h.request("apr_"+code), h.now)
	if err != nil {
		t.Fatalf("redeem %s: %v", code, err)
	}
	return first
}

func approverMissContract(t *testing.T, h *approverHarness) {
	AssertMiss(t, "ApproverDeviceRepository.GetDeviceByTokenHash", func() (*persistence.ApproverDeviceRow, error) {
		return h.repo.GetDeviceByTokenHash(h.ctx, "absent")
	})
	AssertMiss(t, "ApproverDeviceRepository.ResumeDevice", func() (*persistence.ApproverDeviceRow, error) {
		return h.repo.ResumeDevice(h.ctx, "absent", "absent", "absent", h.now)
	})
	AssertMiss(t, "ApproverDeviceRepository.RemintEnrollmentToken", func() (*persistence.ApproverDeviceRow, error) {
		return h.repo.RemintEnrollmentToken(h.ctx, "absent", "absent", h.now)
	})
	AssertMiss(t, "ApproverDeviceRepository.GetPairing", func() (*persistence.ApproverPairingRow, error) {
		return h.repo.GetPairing(h.ctx, "absent", h.now)
	})
	AssertMiss(t, "ApproverDeviceRepository.GetPairingByClaim", func() (*persistence.ApproverPairingRow, error) {
		return h.repo.GetPairingByClaim(h.ctx, "absent")
	})
	AssertMiss(t, "ApproverDeviceRepository.GetRequest", func() (*persistence.AgentApprovalRequestRow, error) {
		return h.repo.GetRequest(h.ctx, "absent")
	})
}

func approverFirstRedeem(t *testing.T, h *approverHarness) {
	h.pairing(t, "a")
	if p, err := h.repo.GetPairing(h.ctx, "code-a", h.now); err != nil || p.Label != "Phone a" {
		t.Fatalf("GetPairing before redemption = %+v, %v", p, err)
	}
	if _, err := h.repo.GetPairing(h.ctx, "code-a", h.now.Add(11*time.Minute)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("GetPairing after expiry: %v", err)
	}
	if !h.redeem(t, "a") {
		t.Fatal("the first redemption on an empty device set was not the first device")
	}
	if _, err := h.repo.GetPairing(h.ctx, "code-a", h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("GetPairing after redemption: %v", err)
	}
	d, err := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_a")
	if err != nil || d.ID != "dev_a" || d.RevokedAt != nil || !d.PairedAt.Equal(h.now) {
		t.Fatalf("device = %+v, %v", d, err)
	}
	if n, _ := h.repo.CountActiveDevices(h.ctx); n != 1 {
		t.Fatalf("active devices = %d, want 1", n)
	}
	if _, err := h.repo.GetRequest(h.ctx, "apr_a"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the first device filed a request: %v", err)
	}
}

func approverLaterRedeem(t *testing.T, h *approverHarness) {
	h.pairing(t, "a")
	h.pairing(t, "b")
	h.redeem(t, "a")
	if h.redeem(t, "b") {
		t.Fatal("a second redemption became a device without approval")
	}
	p, err := h.repo.GetPairingByClaim(h.ctx, "claim-b")
	if err != nil || p.RequestID != "apr_b" || p.DeviceID != "" || p.RedeemedAt == nil {
		t.Fatalf("pending pairing = %+v, %v", p, err)
	}
	r, err := h.repo.GetRequest(h.ctx, "apr_b")
	if err != nil || r.Status != persistence.ApprovalPending || r.Kind != persistence.ApprovalKindDeviceEnrollment {
		t.Fatalf("enrollment request = %+v, %v", r, err)
	}
	if string(r.Rendered) != `{"kind":"device_enrollment","label":"x"}` {
		t.Fatalf("rendered bytes changed in storage: %s", r.Rendered)
	}
	if _, err := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_b"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a pending enrollment created a device: %v", err)
	}
}

func approverRedeemRefusals(t *testing.T, h *approverHarness) {
	redeem := func(code string, at time.Time) error {
		_, err := h.repo.RedeemPairing(h.ctx, "code-"+code, "claim-"+code+at.String(), h.device("dev_"+code+fmt.Sprint(at.Unix())), h.request("apr_"+code+fmt.Sprint(at.Unix())), at)
		return err
	}
	if err := redeem("unknown", h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown code: %v", err)
	}
	h.pairing(t, "x")
	if err := redeem("x", h.now.Add(11*time.Minute)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expired code: %v", err)
	}
	if err := redeem("x", h.now); err != nil {
		t.Fatalf("valid code: %v", err)
	}
	if err := redeem("x", h.now.Add(time.Second)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("reused code: %v", err)
	}
}

// approverConcurrentFirstRedeem is the race the serialisation exists for:
// distinct codes redeemed at once against an empty device set. Exactly one
// may become the first device; the rest must file enrollment requests.
//
// The interleaving is made deterministic through the repository's redeem
// hook: each redeemer, having read the device count, waits until every other
// redeemer has read it too, or until a short timeout. Without serialisation
// all of them read zero inside that window and all become "first", every
// run. With it, each waits out the timeout alone in turn and the count it
// read is the true one.
func approverConcurrentFirstRedeem(t *testing.T, h *approverHarness) {
	const n = 4
	hooked, ok := h.repo.(interface{ SetRedeemHookForTest(func()) })
	if !ok {
		t.Fatalf("%T has no redeem hook; the race cannot be made deterministic", h.repo)
	}
	var arrived sync.WaitGroup
	arrived.Add(n)
	hooked.SetRedeemHookForTest(func() {
		arrived.Done()
		done := make(chan struct{})
		go func() { arrived.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
		}
	})
	defer hooked.SetRedeemHookForTest(nil)
	for i := 0; i < n; i++ {
		h.pairing(t, fmt.Sprint(i))
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first int
		errs  []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(code string) {
			defer wg.Done()
			f, err := h.repo.RedeemPairing(h.ctx, "code-"+code, "claim-"+code, h.device("dev_"+code), h.request("apr_"+code), h.now)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if f {
				first++
			}
		}(fmt.Sprint(i))
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("redemptions failed: %v", errs)
	}
	if first != 1 {
		t.Fatalf("%d redemptions became the first device, want exactly 1", first)
	}
	if c, _ := h.repo.CountActiveDevices(h.ctx); c != 1 {
		t.Fatalf("active devices = %d, want 1", c)
	}
	if p, _ := h.repo.ListPending(h.ctx, h.now); len(p) != n-1 {
		t.Fatalf("pending enrollments = %d, want %d", len(p), n-1)
	}
}

func approverCompleteOnce(t *testing.T, h *approverHarness) {
	h.pairing(t, "a")
	h.pairing(t, "b")
	h.redeem(t, "a")
	h.redeem(t, "b")
	var (
		wg sync.WaitGroup
		mu sync.Mutex
		ok int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := h.device(fmt.Sprintf("dev_b%d", i))
			d.PairedBy = "device:dev_a"
			err := h.repo.CompletePairing(h.ctx, "claim-b", d)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, persistence.ErrNotFound):
				t.Errorf("losing completion: %v, want ErrNotFound", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d completions succeeded, want exactly 1", ok)
	}
	if c, _ := h.repo.CountActiveDevices(h.ctx); c != 2 {
		t.Fatalf("active devices = %d, want 2", c)
	}
	if p, _ := h.repo.GetPairingByClaim(h.ctx, "claim-b"); p == nil || p.DeviceID == "" {
		t.Fatalf("pairing not marked complete: %+v", p)
	}
}

func approverRotateTouchRevoke(t *testing.T, h *approverHarness) {
	h.pairing(t, "a")
	h.redeem(t, "a")
	// Rotation opens a share; the previous value finds the row until it ends
	// (design §9.2, amendment 2026-10-05: P1, devices unpaired by a lost
	// rotation response).
	share := h.now.Add(5 * time.Minute)
	if err := h.repo.RotateToken(h.ctx, "dev_a", "tok-dev_a", "tok-new", "nonce-1", share, h.now); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"tok-dev_a", "tok-new"} {
		d, err := h.repo.GetDeviceByTokenHash(h.ctx, hash)
		if err != nil || d.ID != "dev_a" || d.TokenHash != "tok-new" || d.PrevTokenHash != "tok-dev_a" ||
			d.RotationNonce != "nonce-1" || d.ShareUntil == nil || !d.ShareUntil.Equal(share) {
			t.Fatalf("open share found by %s: %+v, %v", hash, d, err)
		}
	}
	// The compare-and-swap is on the CURRENT value only.
	if err := h.repo.RotateToken(h.ctx, "dev_a", "tok-dev_a", "tok-other", "nonce-2", share, h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("rotation from the previous value wrote: %v, want ErrNotFound", err)
	}
	// Confirming with anything but the current value changes nothing.
	if err := h.repo.ConfirmToken(h.ctx, "dev_a", "tok-dev_a"); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_a"); d == nil || d.PrevTokenHash != "tok-dev_a" {
		t.Fatal("confirming with the previous value ended the share")
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := h.repo.ConfirmToken(h.ctx, "dev_a", "tok-new"); err != nil {
			t.Fatal(err)
		}
	}
	d, err := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_a")
	if err != nil || d.DeadTokenHash != "tok-dev_a" || d.DeadReason != persistence.DeadConfirmed ||
		d.PrevTokenHash != "" || d.RotationNonce != "" || d.ShareUntil != nil {
		t.Fatalf("after confirmation the old value is dead and the nonce erased: %+v, %v", d, err)
	}
	// Rotating from the current value again: the dead value moves on only
	// when that share ends.
	if err := h.repo.RotateToken(h.ctx, "dev_a", "tok-new", "tok-new2", "nonce-3", share, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.ConfirmToken(h.ctx, "dev_a", "tok-new2"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_a"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("a value two rotations back still finds the device")
	}
	later := h.now.Add(time.Hour)
	if err := h.repo.TouchDevice(h.ctx, "dev_a", later); err != nil {
		t.Fatal(err)
	}
	d, err = h.repo.GetDeviceByTokenHash(h.ctx, "tok-new2")
	if err != nil || !d.LastUsedAt.Equal(later) {
		t.Fatalf("after touch: %+v, %v", d, err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := h.repo.RevokeDevice(h.ctx, "dev_a", later); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.repo.RevokeDevice(h.ctx, "dev_unknown", later); err != nil {
		t.Fatal(err)
	}
	if c, _ := h.repo.CountActiveDevices(h.ctx); c != 0 {
		t.Fatalf("active devices after revoke = %d", c)
	}
	d, err = h.repo.GetDeviceByTokenHash(h.ctx, "tok-new2")
	if err != nil || d.RevokedAt == nil {
		t.Fatalf("revoked device: %+v, %v", d, err)
	}
	if err := h.repo.RotateToken(h.ctx, "dev_a", "tok-new2", "tok-5", "nonce-5", later, later); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("a revoked device rotated its token")
	}
	if all, _ := h.repo.ListDevices(h.ctx); len(all) != 1 {
		t.Fatalf("ListDevices = %d rows, want 1 (revoked rows are listed)", len(all))
	}
}

func approverDecide(t *testing.T, h *approverHarness) {
	for i, id := range []string{"apr_1", "apr_2", "apr_3"} {
		r := h.request(id)
		r.CreatedAt = h.now.Add(time.Duration(i) * time.Second)
		if err := h.repo.CreateRequest(h.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if p, _ := h.repo.ListPending(h.ctx, h.now); len(p) != 3 || p[0].ID != "apr_3" {
		t.Fatalf("pending = %v, want 3 newest first", ids(p))
	}
	if err := h.repo.Decide(h.ctx, "apr_1", "sha-WRONG", "dev_a", true, h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("wrong hash: %v", err)
	}
	if err := h.repo.Decide(h.ctx, "apr_1", "sha-apr_1", "dev_a", true, h.now.Add(8*24*time.Hour)); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("after expiry: %v", err)
	}
	if err := h.repo.Decide(h.ctx, "apr_1", "sha-apr_1", "dev_a", true, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.Decide(h.ctx, "apr_1", "sha-apr_1", "dev_a", false, h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("second decision: %v", err)
	}
	if err := h.repo.Decide(h.ctx, "apr_2", "sha-apr_2", "dev_a", false, h.now); err != nil {
		t.Fatal(err)
	}
	r1, _ := h.repo.GetRequest(h.ctx, "apr_1")
	r2, _ := h.repo.GetRequest(h.ctx, "apr_2")
	if r1.Status != persistence.ApprovalApproved || r1.DecidedByDevice != "dev_a" || r1.DecidedAt == nil ||
		r2.Status != persistence.ApprovalRejected {
		t.Fatalf("after decisions: %+v / %+v", r1, r2)
	}
	if p, _ := h.repo.ListPending(h.ctx, h.now); len(p) != 1 || p[0].ID != "apr_3" {
		t.Fatalf("pending after decisions = %v", ids(p))
	}
}

func approverAppliedAndExpiry(t *testing.T, h *approverHarness) {
	for _, id := range []string{"apr_a", "apr_b", "apr_old"} {
		r := h.request(id)
		if id == "apr_old" {
			r.ExpiresAt = h.now.Add(-time.Minute)
		}
		if err := h.repo.CreateRequest(h.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.repo.Decide(h.ctx, "apr_a", "sha-apr_a", "dev_a", true, h.now); err != nil {
		t.Fatal(err)
	}
	if u, _ := h.repo.ListApprovedUnapplied(h.ctx); len(u) != 1 || u[0].ID != "apr_a" {
		t.Fatalf("approved-unapplied = %v", ids(u))
	}
	if err := h.repo.MarkApplyFailed(h.ctx, "apr_a", "file changed", h.now); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.repo.GetRequest(h.ctx, "apr_a"); r.AppliedAt == nil || r.ApplyError != "file changed" {
		t.Fatalf("MarkApplyFailed did not record the terminal failure: %+v", r)
	}
	if err := h.repo.MarkApplied(h.ctx, "apr_a", h.now); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.repo.GetRequest(h.ctx, "apr_a"); r.ApplyError != "file changed" {
		t.Fatalf("a later MarkApplied overwrote a terminal failure: %+v", r)
	}
	if u, _ := h.repo.ListApprovedUnapplied(h.ctx); len(u) != 0 {
		t.Fatalf("after MarkApplied = %v", ids(u))
	}
	if p, _ := h.repo.ListPending(h.ctx, h.now); len(p) != 1 || p[0].ID != "apr_b" {
		t.Fatalf("ListPending shows an expired row: %v", ids(p))
	}
	mine := h.request("apr_ns")
	mine.Namespace = "hermes"
	if err := h.repo.CreateRequest(h.ctx, mine); err != nil {
		t.Fatal(err)
	}
	if got, err := h.repo.ListRecentByNamespace(h.ctx, "hermes", h.now.Add(-time.Hour)); err != nil || len(got) != 1 || got[0].ID != "apr_ns" {
		t.Fatalf("ListRecentByNamespace = %v, %v; want only the hermes request", ids(got), err)
	}
	if got, _ := h.repo.ListRecentByNamespace(h.ctx, "hermes", h.now.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("ListRecentByNamespace ignored since: %v", ids(got))
	}
	n, err := h.repo.ExpirePending(h.ctx, h.now)
	if err != nil || n != 1 {
		t.Fatalf("ExpirePending = %d, %v; want 1", n, err)
	}
	if r, _ := h.repo.GetRequest(h.ctx, "apr_old"); r.Status != persistence.ApprovalExpired {
		t.Fatalf("expired row status = %s", r.Status)
	}
}

// approverClaimApply: review 20261002-4de8 F2. In a cluster every node ticks;
// only the node holding the lease may run an approved request's effect.
func approverClaimApply(t *testing.T, h *approverHarness) {
	pending := h.request("apr_p")
	if err := h.repo.CreateRequest(h.ctx, pending); err != nil {
		t.Fatal(err)
	}
	until := h.now.Add(5 * time.Minute)
	if ok, err := h.repo.ClaimApply(h.ctx, "apr_p", "node-a", until, h.now); err != nil || ok {
		t.Fatalf("claimed a pending request: %v %v", ok, err)
	}
	if err := h.repo.Decide(h.ctx, "apr_p", "sha-apr_p", "dev_a", true, h.now); err != nil {
		t.Fatal(err)
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	start := make(chan struct{})
	for _, node := range []string{"node-a", "node-b", "node-c", "node-d"} {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			<-start
			ok, err := h.repo.ClaimApply(h.ctx, "apr_p", node, until, h.now)
			if err != nil {
				t.Errorf("%s: %v", node, err)
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(node)
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d nodes claimed the same request, want 1", wins)
	}
	r, _ := h.repo.GetRequest(h.ctx, "apr_p")
	if r.ApplyAttempts != 1 {
		t.Fatalf("attempts = %d, want 1", r.ApplyAttempts)
	}
	// A lapsed lease is claimable by another node.
	later := until.Add(time.Second)
	if ok, _ := h.repo.ClaimApply(h.ctx, "apr_p", "node-z", later.Add(5*time.Minute), later); !ok {
		t.Fatal("a lapsed lease could not be reclaimed")
	}
	if err := h.repo.MarkApplied(h.ctx, "apr_p", later); err != nil {
		t.Fatal(err)
	}
	if ok, _ := h.repo.ClaimApply(h.ctx, "apr_p", "node-z", later.Add(time.Hour), later.Add(time.Hour)); ok {
		t.Fatal("an applied request was claimed again")
	}
}

func ids(rows []persistence.AgentApprovalRequestRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// approverShareLifecycle pins the share mechanics on both drivers (design
// §9.2, amendment 2026-10-05): CloseShare only shrinks and only for its own
// nonce; ExpireShares kills the previous value as expired and erases the
// nonce; presenting the successor afterwards relabels it confirmed; the
// streak counts one admission per share, survives confirmation, and resets
// on a share with none.
func approverShareLifecycle(t *testing.T, h *approverHarness) {
	h.pairing(t, "a")
	h.redeem(t, "a")
	far := h.now.Add(5 * time.Minute)
	if err := h.repo.RotateToken(h.ctx, "dev_a", "tok-dev_a", "s1", "n1", far, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CloseShare(h.ctx, "dev_a", "stale-nonce", h.now); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.repo.GetDeviceByTokenHash(h.ctx, "s1"); !d.ShareUntil.Equal(far) {
		t.Fatalf("a stale nonce closed the share: %v", d.ShareUntil)
	}
	soon := h.now.Add(10 * time.Second)
	if err := h.repo.CloseShare(h.ctx, "dev_a", "n1", soon); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CloseShare(h.ctx, "dev_a", "n1", far); err != nil { // never extends
		t.Fatal(err)
	}
	if d, _ := h.repo.GetDeviceByTokenHash(h.ctx, "s1"); !d.ShareUntil.Equal(soon) {
		t.Fatalf("share_until = %v, want %v", d.ShareUntil, soon)
	}
	approverShareStreak(t, h, far)
}

// approverShareStreak continues approverShareLifecycle from an open share
// "n1" (current "s1", previous "tok-dev_a", closing at h.now+10s).
func approverShareStreak(t *testing.T, h *approverHarness, far time.Time) {
	soon := h.now.Add(10 * time.Second)
	// Streak: one admission per share counts once.
	for i, want := range []struct {
		streak int
		first  bool
	}{{1, true}, {1, false}} {
		streak, first, err := h.repo.AdmitShare(h.ctx, "dev_a", "n1")
		if err != nil || streak != want.streak && want.first || first != want.first {
			t.Fatalf("admission %d: streak %d first %v err %v", i, streak, first, err)
		}
	}
	if _, first, _ := h.repo.AdmitShare(h.ctx, "dev_a", "other-nonce"); first {
		t.Fatal("an admission was credited to a share with another nonce")
	}
	// Not yet due: nothing expires.
	if err := h.repo.ExpireShares(h.ctx, h.now); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_a"); d == nil || d.PrevTokenHash != "tok-dev_a" {
		t.Fatal("a share expired before share_until")
	}
	if err := h.repo.ExpireShares(h.ctx, soon); err != nil {
		t.Fatal(err)
	}
	d, err := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_a")
	if err != nil || d.DeadTokenHash != "tok-dev_a" || d.DeadReason != persistence.DeadExpired || d.PrevTokenHash != "" ||
		d.RotationNonce != "" || d.ShareUntil != nil || !d.ShareAdmitted || d.ShareStreak != 1 {
		t.Fatalf("after expiry: %+v, %v", d, err)
	}
	// The successor presented after its share expired: relabelled confirmed.
	if err := h.repo.ConfirmToken(h.ctx, "dev_a", "s1"); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_a"); d.DeadReason != persistence.DeadConfirmed || !d.ShareAdmitted {
		t.Fatalf("relabel: %+v (share_admitted must survive confirmation)", d)
	}
	// The next rotation keeps the streak (the last share admitted a request)
	// and consumes share_admitted; one after a quiet share resets it.
	if err := h.repo.RotateToken(h.ctx, "dev_a", "s1", "s2", "n2", far, h.now); err != nil {
		t.Fatal(err)
	}
	if streak, first, _ := h.repo.AdmitShare(h.ctx, "dev_a", "n2"); !first || streak != 2 {
		t.Fatalf("second consecutive admitted share: streak %d first %v, want 2 true", streak, first)
	}
	if err := h.repo.ConfirmToken(h.ctx, "dev_a", "s2"); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.RotateToken(h.ctx, "dev_a", "s2", "s3", "n3", far, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.ConfirmToken(h.ctx, "dev_a", "s3"); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.RotateToken(h.ctx, "dev_a", "s3", "s4", "n4", far, h.now); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.repo.GetDeviceByTokenHash(h.ctx, "s4"); d.ShareStreak != 0 || d.ShareAdmitted {
		t.Fatalf("a quiet share did not reset the streak: %+v", d)
	}
	// Revocation erases an open share.
	if err := h.repo.RevokeDevice(h.ctx, "dev_a", h.now); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.repo.GetDeviceByTokenHash(h.ctx, "s4"); d.RotationNonce != "" || d.PrevTokenHash != "" {
		t.Fatalf("revocation kept the share: %+v", d)
	}
}

// approverResume pins ResumeDevice: only an expired dead value of an
// unrevoked device, only with an unexpired unused code, both or neither.
func approverResume(t *testing.T, h *approverHarness) {
	h.pairing(t, "a")
	h.redeem(t, "a")
	if err := h.repo.RotateToken(h.ctx, "dev_a", "tok-dev_a", "s1", "n1", h.now.Add(10*time.Second), h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.ExpireShares(h.ctx, h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	h.pairing(t, "r")
	// A wrong dead value: nothing changes, the code is not consumed.
	if _, err := h.repo.ResumeDevice(h.ctx, "code-r", "not-dead", "fresh", h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("resume with an unknown dead value: %v", err)
	}
	if _, err := h.repo.GetPairing(h.ctx, "code-r", h.now); err != nil {
		t.Fatal("a refused resume consumed the code")
	}
	if _, err := h.repo.ResumeDevice(h.ctx, "code-absent", "tok-dev_a", "fresh", h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("resume with an unknown code: %v", err)
	}
	d, err := h.repo.ResumeDevice(h.ctx, "code-r", "tok-dev_a", "fresh", h.now)
	if err != nil || d.ID != "dev_a" || d.TokenHash != "fresh" || d.DeadTokenHash != "" || d.ShareStreak != 0 {
		t.Fatalf("resume: %+v, %v", d, err)
	}
	if _, err := h.repo.GetDeviceByTokenHash(h.ctx, "s1"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("the successor still finds the row after a resume")
	}
	// The code is spent, and the dead value is gone.
	h.pairing(t, "r2")
	if _, err := h.repo.ResumeDevice(h.ctx, "code-r", "tok-dev_a", "x", h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("a used code resumed again")
	}
	if _, err := h.repo.ResumeDevice(h.ctx, "code-r2", "tok-dev_a", "x", h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("a resumed dead value resumed again")
	}
	// A confirmed dead value is not resumable.
	if err := h.repo.RotateToken(h.ctx, "dev_a", "fresh", "s2", "n2", h.now.Add(time.Minute), h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.ConfirmToken(h.ctx, "dev_a", "s2"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.ResumeDevice(h.ctx, "code-r2", "fresh", "x", h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("a confirmed dead value resumed")
	}
	// A revoked device cannot resume.
	if err := h.repo.RotateToken(h.ctx, "dev_a", "s2", "s3", "n3", h.now.Add(time.Second), h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.ExpireShares(h.ctx, h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.RevokeDevice(h.ctx, "dev_a", h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.ResumeDevice(h.ctx, "code-r2", "s2", "x", h.now); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("a revoked device resumed")
	}
}

// approverRemint pins RemintEnrollmentToken (design §9.2, amendment 2026-10-07
// T11, BACKLOG 2026-10-05): every guard, the dead-value swap, and once.
func approverRemint(t *testing.T, h *approverHarness) {
	h.pairing(t, "a")
	h.pairing(t, "b")
	h.redeem(t, "a")
	h.redeem(t, "b")
	notBefore := h.now.Add(-15 * time.Minute)
	// No completed pairing yet: the claim has no device.
	if _, err := h.repo.RemintEnrollmentToken(h.ctx, "claim-b", "n1", notBefore); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("remint before completion: %v", err)
	}
	if _, err := h.repo.RemintEnrollmentToken(h.ctx, "claim-absent", "n1", notBefore); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("remint of an unknown claim: %v", err)
	}
	d := h.device("dev_b")
	if err := h.repo.CompletePairing(h.ctx, "claim-b", d); err != nil {
		t.Fatal(err)
	}
	// Outside the window.
	if _, err := h.repo.RemintEnrollmentToken(h.ctx, "claim-b", "n1", h.now.Add(time.Second)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("remint outside the window: %v", err)
	}
	// Used device.
	if err := h.repo.TouchDevice(h.ctx, "dev_b", h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.RemintEnrollmentToken(h.ctx, "claim-b", "n1", notBefore); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("remint of a used device: %v", err)
	}

	// A second, never-used device: one re-mint, the old value dead as confirmed.
	h.pairing(t, "c")
	h.redeem(t, "c")
	if err := h.repo.CompletePairing(h.ctx, "claim-c", h.device("dev_c")); err != nil {
		t.Fatal(err)
	}
	got, err := h.repo.RemintEnrollmentToken(h.ctx, "claim-c", "n2", notBefore)
	if err != nil || got.ID != "dev_c" || got.TokenHash != "n2" || got.DeadTokenHash != "tok-dev_c" || got.DeadReason != persistence.DeadConfirmed {
		t.Fatalf("remint: %+v, %v", got, err)
	}
	if row, err := h.repo.GetDeviceByTokenHash(h.ctx, "tok-dev_c"); err != nil || row.TokenHash == "tok-dev_c" {
		t.Fatalf("the old value must find the row only as the dead value: %+v, %v", row, err)
	}
	if _, err := h.repo.RemintEnrollmentToken(h.ctx, "claim-c", "n3", notBefore); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a second remint: %v", err)
	}
	// prev_token_hash on its own: a rotation that leaves last_used_at equal to
	// paired_at (the frozen h.now) and dead_token_hash NULL still blocks it.
	h.pairing(t, "e")
	h.redeem(t, "e")
	if err := h.repo.CompletePairing(h.ctx, "claim-e", h.device("dev_e")); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.RotateToken(h.ctx, "dev_e", "tok-dev_e", "s-e", "n-e", h.now.Add(time.Minute), h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.RemintEnrollmentToken(h.ctx, "claim-e", "n5", notBefore); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("remint of a device in a rotation share: %v", err)
	}
	// Revoked device.
	h.pairing(t, "d")
	h.redeem(t, "d")
	if err := h.repo.CompletePairing(h.ctx, "claim-d", h.device("dev_d")); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.RevokeDevice(h.ctx, "dev_d", h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.RemintEnrollmentToken(h.ctx, "claim-d", "n4", notBefore); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("remint of a revoked device: %v", err)
	}
}

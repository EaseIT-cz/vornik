package approverdevice

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// BACKLOG 2026-10-05 (design §9.2, amendment 2026-10-07 T11): the re-mint of
// an enrollment token whose poll response was lost.

// approvedClaim pairs a first device, redeems a second code and has the first
// device approve it. The second device's claim token is returned unpolled.
func approvedClaim(t *testing.T, f *fixture) string {
	t.Helper()
	ctx := context.Background()
	first, _ := f.firstDevice(t)
	r, err := f.svc.Redeem(ctx, f.pair(t, "Tablet"), "10.0.0.2")
	if err != nil || r.ClaimToken == "" {
		t.Fatalf("redeem = %+v, %v", r, err)
	}
	pend, _ := f.svc.ListPending(ctx)
	if len(pend) != 1 {
		t.Fatalf("pending: %+v", pend)
	}
	if err := f.svc.Decide(ctx, first, pend[0].ID, pend[0].RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	return r.ClaimToken
}

func pollOK(t *testing.T, f *fixture, claim string) string {
	t.Helper()
	tok, st, err := f.svc.PollClaim(context.Background(), claim)
	if err != nil || st != ClaimApproved || tok == "" {
		t.Fatalf("poll = %q %s %v, want approved", tok, st, err)
	}
	return tok
}

func pollExpired(t *testing.T, f *fixture, claim, why string) {
	t.Helper()
	if tok, st, err := f.svc.PollClaim(context.Background(), claim); err != nil || st != ClaimExpired || tok != "" {
		t.Fatalf("%s: poll = %q %s %v, want expired", why, tok, st, err)
	}
}

func authOK(t *testing.T, f *fixture, tok string) {
	t.Helper()
	if _, err := f.svc.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
}

func authDead(t *testing.T, f *fixture, tok, who string) {
	t.Helper()
	if _, err := f.svc.Authenticate(context.Background(), tok); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("%s authenticates: %v", who, err)
	}
}

func TestRemint_UsedDeviceIsNotReminted(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	f.advance(time.Second)
	authOK(t, f, t1) // the first presentation touches last_used_at at once
	pollExpired(t, f, claim, "a used device")
	authOK(t, f, t1)
}

func TestRemint_AfterTheWindowIsExpired(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	f.advance(ClaimTTL + time.Second)
	pollExpired(t, f, claim, "after the claim window")
	_ = t1
}

func TestRemint_RevokedDeviceIsExpired(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	pollOK(t, f, claim)
	devs, _ := f.svc.ListDevices(context.Background())
	for _, d := range devs {
		if d.Label == "Tablet" {
			if err := f.svc.Revoke(context.Background(), d.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	pollExpired(t, f, claim, "a revoked device")
}

func TestRemint_SecondRemintIsExpired(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	pollOK(t, f, claim)
	t2 := pollOK(t, f, claim) // the re-mint
	pollExpired(t, f, claim, "a second re-mint")
	authOK(t, f, t2)
}

// Two polls with the same claim, one after the other (the serialized form of
// a double mint): one live token, one dead. The truly parallel case collapses
// to this outcome through the single guarded statement.
func TestRemint_TwoPollsOneLiveTokenOneDead(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	t2 := pollOK(t, f, claim)
	if t1 == t2 {
		t.Fatal("the re-mint returned the same token")
	}
	authDead(t, f, t1, "the first token")
	authOK(t, f, t2)
}

func TestRemint_RaisesAnAlertWithoutASecret(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	before := f.countPushesContaining("re-issued")
	t2 := pollOK(t, f, claim)
	if n := f.countPushesContaining("re-issued") - before; n != 1 {
		t.Fatalf("%d re-mint alerts, want 1", n)
	}
	f.assertNoSecretsPushed(t, claim, t1, t2)
}

func TestRemint_FirstAuthenticationTouchesLastUsed(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	f.advance(time.Minute) // well inside touchInterval
	authOK(t, f, t1)
	for _, d := range mustDevices(t, f) {
		if d.Label == "Tablet" && !d.LastUsedAt.After(d.PairedAt) {
			t.Fatalf("last_used_at %v not after paired_at %v after the first authentication", d.LastUsedAt, d.PairedAt)
		}
	}
}

// The thief authenticates before the owner's re-poll: the owner is refused
// through the last_used_at guard; the thief's token stays live; the
// enrollment push (not a re-mint alert) is the signal.
func TestRemint_ThiefWhoAuthenticatesLocksTheOwnerOut(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim) // the thief, holding a copy of the claim cookie
	f.advance(time.Second)
	authOK(t, f, t1)
	pollExpired(t, f, claim, "the owner after the thief authenticated")
	authOK(t, f, t1)
	if n := f.countPushesContaining("new approver device was paired"); n < 1 {
		t.Fatalf("no enrollment push (%d)", n)
	}
	f.assertNoPushContaining(t, "re-issued")
	f.assertNoSecretsPushed(t, claim, t1)
}

// The thief only polls: the owner's re-poll re-mints and kills the thief's token.
func TestRemint_ThiefWhoOnlyPollsDoesNotStopTheOwner(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	t2 := pollOK(t, f, claim)
	authDead(t, f, t1, "the thief's token")
	authOK(t, f, t2)
	if n := f.countPushesContaining("re-issued"); n != 1 {
		t.Fatalf("%d re-mint alerts, want 1", n)
	}
}

// Rotation after a re-mint: the re-mint's old token stays dead, rotation
// proceeds, and a further re-mint is refused.
func TestRemint_RotationAfterRemint(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	t2 := pollOK(t, f, claim)
	dev, err := f.svc.Authenticate(context.Background(), t2)
	if err != nil {
		t.Fatal(err)
	}
	t3, err := f.svc.Rotate(context.Background(), dev, t2)
	if err != nil || t3 == "" {
		t.Fatalf("rotate = %q, %v", t3, err)
	}
	authDead(t, f, t1, "the re-mint's old token")
	authOK(t, f, t3)
	pollExpired(t, f, claim, "a re-mint after a rotation")
}

// A re-mint-discarded token is `confirmed`: a fresh pairing code does not
// resume it; the further-device path (another device's approval) applies.
func TestRemint_DiscardedTokenCannotBeResumedWithACode(t *testing.T) {
	f := newFixture(t)
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	t2 := pollOK(t, f, claim)
	if _, _, err := f.svc.Resume(context.Background(), f.pair(t, "Tablet"), t1, "10.0.0.9"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("resume with the discarded token: %v, want ErrBadCode", err)
	}
	authOK(t, f, t2)
	// A fresh code on a further device still needs another device's approval.
	r, err := f.svc.Redeem(context.Background(), f.pair(t, "Third"), "10.0.0.3")
	if err != nil || r.DeviceToken != "" || r.ClaimToken == "" {
		t.Fatalf("further device redeem = %+v, %v", r, err)
	}
}

func mustDevices(t *testing.T, f *fixture) []persistence.ApproverDeviceRow {
	t.Helper()
	d, err := f.svc.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// failTouch fails TouchDevice while fail is set.
type failTouch struct {
	persistence.ApproverDeviceRepository
	fail bool
}

func (r *failTouch) TouchDevice(ctx context.Context, id string, now time.Time) error {
	if r.fail {
		return errors.New("touch: disk full")
	}
	return r.ApproverDeviceRepository.TouchDevice(ctx, id, now)
}

// Security review aace: a failed first-presentation touch must refuse the
// request (the device would still read never-used and could be re-minted
// away); once a touch succeeds, a re-mint is impossible.
func TestRemint_FailedFirstTouchRefusesAndBlocksNoRemint(t *testing.T) {
	f := newFixture(t)
	stub := &failTouch{ApproverDeviceRepository: f.repo}
	f.svc = New(stub, WithClock(f.clock), WithOrigin("https://vornik.example/"))
	claim := approvedClaim(t, f)
	t1 := pollOK(t, f, claim)
	f.advance(time.Second)
	stub.fail = true
	if _, err := f.svc.Authenticate(context.Background(), t1); err == nil {
		t.Fatal("a request was served although the first touch failed")
	}
	stub.fail = false
	authOK(t, f, t1) // the touch now succeeds
	pollExpired(t, f, claim, "a re-mint after a successful touch")
}

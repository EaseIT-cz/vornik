package tradingauth

import (
	"testing"
	"time"
)

// GitHub #444 (found at review; trading-internal-auth design, Amendment
// 2026-10-31): the nonce cache is in memory, so a request captured before a
// daemon restart could be replayed once after it. A signature timestamped
// before the verifier started is now refused.

func TestVerify_SignedBeforeStartIsRefused_444(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	s := NewSigner(testSecret)
	// Captured 30s before the restart: fresh, valid, nonce unseen by the
	// new process.
	captured := newSignedRequest(t, s, `{"id":"o1"}`, start.Add(-30*time.Second))
	v := NewVerifierAt(testSecret, DefaultClockSkew, start)
	err := v.Verify(captured, []byte(`{"id":"o1"}`), start.Add(5*time.Second))
	if err == nil {
		t.Fatal("a request signed before the verifier started must be refused")
	}
	if ve, ok := err.(*VerifyError); !ok || ve.Reason != "signed before this daemon started" {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

// Signatures carry whole seconds: one signed in the very second the verifier
// started (its timestamp floors to the start second) still verifies.
func TestVerify_SignedInTheStartSecondVerifies_444(t *testing.T) {
	start := time.Unix(1_700_000_000, 700_000_000)
	s := NewSigner(testSecret)
	req := newSignedRequest(t, s, `{}`, start.Add(100*time.Millisecond))
	v := NewVerifierAt(testSecret, DefaultClockSkew, start)
	if err := v.Verify(req, []byte(`{}`), start.Add(time.Second)); err != nil {
		t.Fatalf("a request signed in the start second must verify: %v", err)
	}
}

func TestVerify_ReplayAfterStartStillRefused_444(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	s := NewSigner(testSecret)
	req := newSignedRequest(t, s, `{}`, start.Add(10*time.Second))
	v := NewVerifierAt(testSecret, DefaultClockSkew, start)
	if err := v.Verify(req, []byte(`{}`), start.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(req, []byte(`{}`), start.Add(12*time.Second)); err == nil {
		t.Fatal("a nonce replayed after start must still be refused")
	}
}

// NewVerifier starts now: a request signed a minute ago is refused.
func TestNewVerifier_StartsNow_444(t *testing.T) {
	s := NewSigner(testSecret)
	req := newSignedRequest(t, s, `{}`, time.Now().Add(-time.Minute))
	if err := NewVerifier(testSecret, DefaultClockSkew).Verify(req, []byte(`{}`), time.Now()); err == nil {
		t.Fatal("NewVerifier must refuse a request signed before it was built")
	}
}

package approverdevice

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type ctxKey795 struct{}

// GitHub #795 (part 22 §22.2.16c item 1). Found at: production (2026-11-16
// and 2026-11-17): the phone's request was the effect's context, and the
// client leaving mid-reload cancelled an approved change after its files
// were written. Regression test at: integration, the real Service against a
// real SQLite repository; the end-to-end shape is
// TestApprovedApply_CancelledRequestStillApplies_795 in internal/service.
func TestClaimAndApply_EffectDetachedFromCaller_795(t *testing.T) {
	f := newFixture(t)
	d, _ := f.firstDevice(t)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey795{}, "kept"))
	var sawErr error
	var sawValue any
	var deadline time.Duration
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(ectx context.Context, _ persistence.AgentApprovalRequestRow) error {
		cancel() // the client leaves while the effect runs
		sawErr = ectx.Err()
		sawValue = ectx.Value(ctxKey795{})
		if dl, ok := ectx.Deadline(); ok {
			deadline = time.Until(dl)
		}
		return nil
	})
	if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: "apr_795", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Decide(ctx, d, "apr_795", "h", true); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if sawErr != nil {
		t.Fatalf("the effect's context was cancelled with its caller: %v", sawErr)
	}
	if sawValue != "kept" {
		t.Fatalf("the caller's values did not carry over: %v", sawValue)
	}
	if deadline <= 0 || deadline > approvalEffectTimeout {
		t.Fatalf("effect deadline %v, want a bound no later than %v", deadline, approvalEffectTimeout)
	}
	r, err := f.repo.GetRequest(context.Background(), "apr_795")
	if err != nil {
		t.Fatal(err)
	}
	if r.AppliedAt == nil {
		t.Fatal("the applied mark was lost with the cancelled caller")
	}
}

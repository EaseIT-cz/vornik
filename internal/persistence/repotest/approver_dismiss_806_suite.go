package repotest

import (
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #806 (agent-administered design part 22 §22.2.16d, part 06 §6.5
// amendment 2026-11-17): a failed apply could not be cleared from the phone
// or the console, and a withheld-tools notice had no action. Found at:
// production (operator).

// approverFailed806 files an approved request whose apply failed with reason.
func (h *approverHarness) approverFailed806(t *testing.T, id, reason string) {
	t.Helper()
	r := h.request(id)
	r.CreatedAt = h.now.Add(-time.Hour)
	r.ExpiresAt = r.CreatedAt.Add(7 * 24 * time.Hour)
	if err := h.repo.CreateRequest(h.ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.Decide(h.ctx, id, "sha-"+id, "dev_a", true, r.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.MarkApplyFailed(h.ctx, id, reason, h.now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func approverDismiss806(t *testing.T, h *approverHarness) {
	h.approverFailed806(t, "apr_d1", "approverdevice: the change cannot be applied: x")
	h.approverFailed806(t, "apr_d2", "gave up after 60 attempts: busy")
	pending := h.request("apr_pend")
	if err := h.repo.CreateRequest(h.ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.DismissApplyFailed(h.ctx, "apr_d1", "device:dev_a", h.now); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	got, err := h.repo.ListApplyFailedSince(h.ctx, h.now.Add(-7*24*time.Hour), 20)
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(got); len(g) != 1 || g[0] != "apr_d2" {
		t.Fatalf("failed list after a dismissal = %v, want [apr_d2]", g)
	}
	if err := h.repo.DismissApplyFailed(h.ctx, "apr_d1", "operator:other", h.now.Add(time.Minute)); err != nil {
		t.Fatalf("a second dismissal must be a no-op, got %v", err)
	}
	r, err := h.repo.GetRequest(h.ctx, "apr_d1")
	if err != nil {
		t.Fatal(err)
	}
	if r.DismissedBy != "device:dev_a" || r.DismissedAt == nil || !r.DismissedAt.Equal(h.now) {
		t.Errorf("dismissal record = %q %v, want the first dismissal kept", r.DismissedBy, r.DismissedAt)
	}
	if r.Status != persistence.ApprovalApproved || r.ApplyError == "" {
		t.Errorf("a dismissal changed the record of what failed: %+v", r)
	}
	if err := h.repo.DismissApplyFailed(h.ctx, "apr_pend", "device:dev_a", h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Errorf("dismissing a pending request = %v, want ErrApprovalNoTransition", err)
	}
}

func approverRetryReset806(t *testing.T, h *approverHarness) {
	const prefix = "gave up after "
	h.approverFailed806(t, "apr_gave", "gave up after 60 attempts: busy")
	h.approverFailed806(t, "apr_perm", "approverdevice: the change cannot be applied: x")
	h.approverFailed806(t, "apr_dis", "gave up after 60 attempts: busy")
	if err := h.repo.DismissApplyFailed(h.ctx, "apr_dis", "device:dev_a", h.now); err != nil {
		t.Fatal(err)
	}
	h.approverFailed806(t, "apr_exp", "gave up after 60 attempts: busy")
	for id, why := range map[string]string{"apr_perm": "a permanent failure", "apr_dis": "a dismissed row"} {
		if err := h.repo.ResetForRetry(h.ctx, id, prefix, "device:dev_a", h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
			t.Errorf("retry of %s = %v, want ErrApprovalNoTransition", why, err)
		}
	}
	if err := h.repo.ResetForRetry(h.ctx, "apr_exp", prefix, "device:dev_a", h.now.Add(8*24*time.Hour)); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Errorf("retry past the request's expiry = %v, want ErrApprovalNoTransition", err)
	}
	if err := h.repo.ResetForRetry(h.ctx, "apr_gave", prefix, "device:dev_a", h.now); err != nil {
		t.Fatalf("retry of a given-up row: %v", err)
	}
	r, err := h.repo.GetRequest(h.ctx, "apr_gave")
	if err != nil {
		t.Fatal(err)
	}
	if r.AppliedAt != nil || r.ApplyError != "" || r.ApplyAttempts != 0 || r.RetriedBy != "device:dev_a" || r.RetriedAt == nil {
		t.Fatalf("reset row = applied %v error %q attempts %d retried %q %v", r.AppliedAt, r.ApplyError, r.ApplyAttempts, r.RetriedBy, r.RetriedAt)
	}
	unapplied, err := h.repo.ListApprovedUnapplied(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(unapplied); len(g) != 1 || g[0] != "apr_gave" {
		t.Fatalf("approved-unapplied after the reset = %v, want [apr_gave]", g)
	}
	// A failed row under a live lease is not reachable through this API:
	// MarkApplyFailed clears the lease. The reset's lease predicate is defence
	// in depth (design §22.2.16d item 3).
}

func approverNoticeAcks806(t *testing.T, h *approverHarness) {
	for _, k := range []string{"k1", "k2", "k1"} {
		if err := h.repo.AckNotice(h.ctx, k, "device:dev_a", h.now); err != nil {
			t.Fatalf("ack %s: %v", k, err)
		}
	}
	got, err := h.repo.ListNoticeAcks(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["k1"] || !got["k2"] {
		t.Fatalf("acks = %v, want k1 and k2", got)
	}
	if err := h.repo.PruneNoticeAcks(h.ctx, []string{"k2", "k3"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = h.repo.ListNoticeAcks(h.ctx); len(got) != 1 || !got["k2"] {
		t.Fatalf("acks after pruning to the live notices = %v, want k2", got)
	}
	if err := h.repo.PruneNoticeAcks(h.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = h.repo.ListNoticeAcks(h.ctx); len(got) != 0 {
		t.Fatalf("acks with no live notice = %v, want none", got)
	}
}

package repotest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #264 (2026-10-13): the daemon could end a pending approval request
// only by a device decision or the expiry tick, so a request it knew was moot
// (the §18.4 F10 cover request once budgets were lowered back) still asked the
// operator a question whose answer was "nothing changed". WithdrawRequest is
// one statement guarded on status = 'pending', the same guard Decide and the
// expiry tick use, so exactly one of them transitions a row on both drivers
// (agent-administered design §9.2d).
func approverWithdraw(t *testing.T, h *approverHarness) {
	h.withdrawable(t, "apr_w", "apr_d")

	if err := h.repo.WithdrawRequest(h.ctx, "apr_w", wdBy, wdWhy, h.now); err != nil {
		t.Fatalf("withdraw a pending row: %v", err)
	}
	w, err := h.repo.GetRequest(h.ctx, "apr_w")
	if err != nil || w.Status != persistence.ApprovalWithdrawn || w.WithdrawnBy != wdBy || w.WithdrawReason != wdWhy ||
		w.DecidedAt == nil || !w.DecidedAt.Equal(h.now) || w.DecidedByDevice != "" {
		t.Fatalf("withdrawn row = %+v, %v", w, err)
	}
	if p, _ := h.repo.ListPending(h.ctx, h.now); len(p) != 1 || p[0].ID != "apr_d" {
		t.Fatalf("pending after the withdrawal = %v, want only apr_d", ids(p))
	}
	// Idempotent at the repository: a second withdrawal is no transition and
	// changes nothing; neither does a decision after it.
	if err := h.repo.WithdrawRequest(h.ctx, "apr_w", "system:other", "Another reason.", h.now.Add(time.Minute)); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("second withdrawal: %v", err)
	}
	if err := h.repo.Decide(h.ctx, "apr_w", "sha-apr_w", "dev_a", true, h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("decide after withdrawal: %v", err)
	}
	if again, _ := h.repo.GetRequest(h.ctx, "apr_w"); again.Status != persistence.ApprovalWithdrawn || again.WithdrawnBy != wdBy ||
		again.WithdrawReason != wdWhy || !again.DecidedAt.Equal(h.now) {
		t.Fatalf("a refused call changed the withdrawn row: %+v", again)
	}
	// Decided first: the withdrawal is refused and the decision stands.
	if err := h.repo.Decide(h.ctx, "apr_d", "sha-apr_d", "dev_a", true, h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.WithdrawRequest(h.ctx, "apr_d", wdBy, wdWhy, h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("withdraw after decide: %v", err)
	}
	if d, _ := h.repo.GetRequest(h.ctx, "apr_d"); d.Status != persistence.ApprovalApproved || d.WithdrawReason != "" || d.WithdrawnBy != "" {
		t.Fatalf("decided row after a refused withdrawal = %+v", d)
	}
	if err := h.repo.WithdrawRequest(h.ctx, "apr_unknown", wdBy, wdWhy, h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("withdraw an unknown row: %v", err)
	}
	// Recent rows carry the withdrawal for list_my_setup.
	recent, err := h.repo.ListRecentByNamespace(h.ctx, "hermes", h.now.Add(-time.Hour))
	if err != nil || len(recent) != 2 {
		t.Fatalf("ListRecentByNamespace = %v, %v", ids(recent), err)
	}
	for _, r := range recent {
		if r.ID == "apr_w" && (r.Status != persistence.ApprovalWithdrawn || r.WithdrawReason != wdWhy) {
			t.Fatalf("ListRecentByNamespace lost the withdrawal: %+v", r)
		}
	}
}

const wdBy, wdWhy = "system:agent-admin-cover", "Within the limit. Nothing needs approving."

// withdrawable files pending widening_change requests in namespace hermes;
// an id ending in "_late" or "_x" is past its TTL, the tick not yet run.
func (h *approverHarness) withdrawable(t *testing.T, idList ...string) {
	t.Helper()
	for _, id := range idList {
		r := h.request(id)
		r.Kind = persistence.ApprovalKindWideningChange
		r.Namespace = "hermes"
		if strings.HasSuffix(id, "_late") || strings.HasSuffix(id, "_x") {
			r.ExpiresAt = h.now.Add(-time.Minute)
		}
		if err := h.repo.CreateRequest(h.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

// approverWithdrawVsExpiry, review 0df6 F4: a row past its TTL withdrawn
// before the tick stays withdrawn; one the tick expired first cannot be
// withdrawn. Both statements guard on pending: exactly one transition each.
func approverWithdrawVsExpiry(t *testing.T, h *approverHarness) {
	h.withdrawable(t, "apr_late", "apr_x")
	if err := h.repo.WithdrawRequest(h.ctx, "apr_late", wdBy, wdWhy, h.now); err != nil {
		t.Fatalf("withdraw a past-TTL pending row: %v", err)
	}
	expired, err := h.repo.ExpirePendingRows(h.ctx, h.now)
	if err != nil || len(expired) != 1 || expired[0].ID != "apr_x" {
		t.Fatalf("expiry tick = %v, %v; want only apr_x", ids(expired), err)
	}
	if late, _ := h.repo.GetRequest(h.ctx, "apr_late"); late.Status != persistence.ApprovalWithdrawn {
		t.Fatalf("the tick expired a withdrawn row: %s", late.Status)
	}
	if err := h.repo.WithdrawRequest(h.ctx, "apr_x", wdBy, wdWhy, h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("withdraw an expired row: %v", err)
	}
}

// approverWithdrawRefusesBadInput: the rule on who and why lives in the
// repository, so every caller (the daemon, vornikctl) is held to it (review
// 0df6 F1). Nothing is written on a refusal.
func approverWithdrawRefusesBadInput(t *testing.T, h *approverHarness) {
	r := h.request("apr_bad")
	r.Kind = persistence.ApprovalKindWideningChange
	if err := h.repo.CreateRequest(h.ctx, r); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"empty by":       {"", "A reason."},
		"empty reason":   {"system:x", "   "},
		"long reason":    {"system:x", strings.Repeat("a", 301)},
		"control reason": {"system:x", "line one\nline two"},
	} {
		if err := h.repo.WithdrawRequest(h.ctx, "apr_bad", c[0], c[1], h.now); !errors.Is(err, persistence.ErrBadWithdrawal) {
			t.Errorf("%s: %v, want ErrBadWithdrawal", name, err)
		}
	}
	if got, _ := h.repo.GetRequest(h.ctx, "apr_bad"); got.Status != persistence.ApprovalPending {
		t.Fatalf("a refused withdrawal wrote: %s", got.Status)
	}
}

package repotest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The Hermes approval transport's persistence (design
// https://docs.vornik.io §4.1,
// §4.2, §4.4), on both drivers through RunApproverDeviceSuite.

func (h *approverHarness) hostAction(id, ns string, created time.Time) persistence.AgentApprovalRequestRow {
	return persistence.AgentApprovalRequestRow{ID: id, Namespace: ns, Kind: persistence.ApprovalKindHostAction,
		Sentence: "Hermes (" + ns + ") wants to run a command its safety rules flagged: recursive delete.",
		Rendered: []byte(`{"kind":"host_action","request_id":"` + id + `"}`), RenderedSHA256: "sha-" + id,
		Status: persistence.ApprovalPending, CreatedAt: created, ExpiresAt: created.Add(5 * time.Minute)}
}

// Design §4.1 and §4.2: the host_action kind is accepted (the kind CHECK
// is widened), and a decision records its scope in decided_choice in the
// same guarded statement as the status. Every other kind's decision leaves
// it empty.
func approverHostActionChoice(t *testing.T, h *approverHarness) {
	for _, id := range []string{"apr_h1", "apr_h2"} {
		if err := h.repo.CreateRequest(h.ctx, h.hostAction(id, "hermes", h.now)); err != nil {
			t.Fatalf("a host_action request was refused: %v", err)
		}
	}
	if err := h.repo.DecideWithChoice(h.ctx, "apr_h1", "sha-apr_h1", "dev_a", true, "session", h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.DecideWithChoice(h.ctx, "apr_h1", "sha-apr_h1", "dev_a", false, "deny", h.now); !errors.Is(err, persistence.ErrApprovalNoTransition) {
		t.Fatalf("a second decision with a choice: %v", err)
	}
	got, err := h.repo.GetRequest(h.ctx, "apr_h1")
	if err != nil || got.Kind != persistence.ApprovalKindHostAction || got.Status != persistence.ApprovalApproved ||
		got.DecidedChoice != "session" || got.DecidedByDevice != "dev_a" {
		t.Fatalf("after DecideWithChoice: %+v %v", got, err)
	}
	if err := h.repo.DecideWithChoice(h.ctx, "apr_h2", "sha-apr_h2", "dev_a", false, "deny", h.now); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.repo.GetRequest(h.ctx, "apr_h2"); got.Status != persistence.ApprovalRejected || got.DecidedChoice != "deny" {
		t.Fatalf("a deny: %+v", got)
	}
	plain := h.request("apr_plain")
	if err := h.repo.CreateRequest(h.ctx, plain); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.Decide(h.ctx, "apr_plain", "sha-apr_plain", "dev_a", true, h.now); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.repo.GetRequest(h.ctx, "apr_plain"); got.DecidedChoice != "" {
		t.Fatalf("Decide wrote a choice: %q", got.DecidedChoice)
	}
}

// Design §4.4: at most MaxPending pending requests of a kind per namespace
// and MaxRecent filed since Since. Over either nothing is written; another
// namespace is not counted.
func approverCappedCreate(t *testing.T, h *approverHarness) {
	limit := persistence.ApprovalCap{MaxPending: 3, MaxRecent: 4, Since: h.now.Add(-time.Hour)}
	for i := 0; i < 3; i++ {
		if err := h.repo.CreateRequestCapped(h.ctx, h.hostAction(fmt.Sprintf("apr_c%d", i), "hermes", h.now), limit, h.now); err != nil {
			t.Fatalf("filing %d under the cap: %v", i, err)
		}
	}
	if err := h.repo.CreateRequestCapped(h.ctx, h.hostAction("apr_c3", "hermes", h.now), limit, h.now); !errors.Is(err, persistence.ErrApprovalCapPending) {
		t.Fatalf("a fourth pending: %v, want ErrApprovalCapPending", err)
	}
	if _, err := h.repo.GetRequest(h.ctx, "apr_c3"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a refused filing was written: %v", err)
	}
	// Another namespace has its own count.
	if err := h.repo.CreateRequestCapped(h.ctx, h.hostAction("apr_other", "codex", h.now), limit, h.now); err != nil {
		t.Fatalf("another namespace was counted: %v", err)
	}
	// An expired row no longer counts as pending, but it counts as recent.
	later := h.now.Add(6 * time.Minute)
	if _, err := h.repo.ExpirePendingRows(h.ctx, later); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CreateRequestCapped(h.ctx, h.hostAction("apr_c4", "hermes", later), limit, later); err != nil {
		t.Fatalf("after expiry, under the hourly cap: %v", err)
	}
	if err := h.repo.CreateRequestCapped(h.ctx, h.hostAction("apr_c5", "hermes", later), limit, later); !errors.Is(err, persistence.ErrApprovalCapRecent) {
		t.Fatalf("a fifth in the hour: %v, want ErrApprovalCapRecent", err)
	}
	// A duplicate id is refused, never counted twice or overwritten.
	roomy := persistence.ApprovalCap{MaxPending: 99, MaxRecent: 99, Since: limit.Since}
	if err := h.repo.CreateRequestCapped(h.ctx, h.hostAction("apr_c4", "hermes", later), roomy, later); err == nil {
		t.Fatal("a duplicate id was accepted")
	}
}

// Design §4.4: "a count in a single transaction on both drivers", proved
// with two concurrent filings at the limit: exactly one is written. The
// count hook holds each filing after its count until the other has had the
// chance to count too, so an unserialised count is caught every run.
func approverCappedCreateConcurrent(t *testing.T, h *approverHarness) {
	limit := persistence.ApprovalCap{MaxPending: 3, MaxRecent: 30, Since: h.now.Add(-time.Hour)}
	for i := 0; i < 2; i++ {
		if err := h.repo.CreateRequestCapped(h.ctx, h.hostAction(fmt.Sprintf("apr_s%d", i), "hermes", h.now), limit, h.now); err != nil {
			t.Fatal(err)
		}
	}
	hooked, ok := h.repo.(interface{ SetRedeemHookForTest(func()) })
	if !ok {
		t.Fatalf("%T has no count hook; the race cannot be made deterministic", h.repo)
	}
	const n = 2
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
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		written int
		other   []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			err := h.repo.CreateRequestCapped(h.ctx, h.hostAction(id, "hermes", h.now), limit, h.now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				written++
			case errors.Is(err, persistence.ErrApprovalCapPending):
			default:
				other = append(other, err)
			}
		}(fmt.Sprintf("apr_race%d", i))
	}
	wg.Wait()
	if len(other) > 0 {
		t.Fatalf("filings failed: %v", other)
	}
	if written != 1 {
		t.Fatalf("%d concurrent filings at the limit were written, want exactly 1", written)
	}
	if p, _ := h.repo.ListPending(h.ctx, h.now); len(p) != 3 {
		t.Fatalf("pending = %d, want the cap of 3", len(p))
	}
}

// Design §8 (observable): the expiry tick reports which rows it expired, so
// the host_action outcome "expired" is counted once per row.
func approverExpirePendingRows(t *testing.T, h *approverHarness) {
	if err := h.repo.CreateRequest(h.ctx, h.hostAction("apr_e1", "hermes", h.now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CreateRequest(h.ctx, h.hostAction("apr_e2", "hermes", h.now)); err != nil {
		t.Fatal(err)
	}
	rows, err := h.repo.ExpirePendingRows(h.ctx, h.now)
	if err != nil || len(rows) != 1 || rows[0].ID != "apr_e1" || rows[0].Kind != persistence.ApprovalKindHostAction || rows[0].Status != persistence.ApprovalExpired {
		t.Fatalf("ExpirePendingRows = %+v, %v; want apr_e1, expired", rows, err)
	}
	if rows, _ := h.repo.ExpirePendingRows(h.ctx, h.now); len(rows) != 0 {
		t.Fatalf("a second pass expired %v again", ids(rows))
	}
}

package approverdevice

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #579 (agent-administered design part 22, §22.2.16a). Found at:
// production (2026-10-28): two approved widenings failed transiently against
// a busy account transition and were retried every minute, about 200 times
// each, while the agent saw them pending and the phone showed nothing.
// Regression test at: integration, the real Service against a real SQLite
// repository. Lower because: production needed the busy transition; the retry
// loop is exactly this code.
func TestReapply_579_GivesUpAtTheCeiling(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	var terminal []string
	f.svc.RegisterTerminalObserver(persistence.ApprovalKindWideningChange, func(_ context.Context, r persistence.AgentApprovalRequestRow, status string) {
		terminal = append(terminal, status+":"+r.ApplyError)
	})
	calls := 0
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error {
		calls++
		return errors.New("persistence: account transition is busy or stale")
	})
	if err := f.repo.CreateRequest(ctx, persistence.AgentApprovalRequestRow{ID: "apr_c", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "s", Rendered: []byte(`{}`), RenderedSHA256: "h", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	_ = f.svc.Decide(ctx, d, "apr_c", "h", true) // attempt 1
	for calls < approvalApplyCeiling-1 {
		f.advance(applyLease + time.Second)
		_ = f.svc.Tick(ctx)
	}
	if len(terminal) != 0 {
		t.Fatalf("below the ceiling (%d attempts) the request must stay approved-unapplied, got terminal %v", calls, terminal)
	}
	if rows, err := f.repo.ListApprovedUnapplied(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("approved-unapplied = %d (%v), want 1", len(rows), err)
	}
	f.advance(applyLease + time.Second)
	_ = f.svc.Tick(ctx) // attempt 60
	if calls != approvalApplyCeiling {
		t.Fatalf("effect ran %d times, want %d", calls, approvalApplyCeiling)
	}
	if len(terminal) != 1 || !strings.HasPrefix(terminal[0], "failed:") || !strings.Contains(terminal[0], "gave up after 60 attempts") {
		t.Fatalf("at the ceiling the request must end failed once, naming the ceiling: %v", terminal)
	}
	if rows, err := f.repo.ListApprovedUnapplied(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("a request given up on must not be retried: %d (%v)", len(rows), err)
	}
	f.advance(applyLease + time.Second)
	_ = f.svc.Tick(ctx)
	if calls != approvalApplyCeiling || len(terminal) != 1 {
		t.Fatalf("after giving up: %d calls, %d terminal notices", calls, len(terminal))
	}
}

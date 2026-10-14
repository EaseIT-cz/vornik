package approverdevice

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #264 (2026-10-13): the daemon could end a pending request only by a
// device decision or the TTL, so a request it knew was moot (the §18.4 F10
// cover request once budgets were lowered back) still asked the operator a
// question whose answer was "nothing changed". Agent-administered design
// §9.2d: Withdraw, opt-in per kind, idempotent, race-safe against a decision.

const (
	wBy     = "system:agent-admin-cover"
	wReason = "Your assistant's projects now add up to $8 a month, within the $8 limit. Nothing needs approving."
)

// withdrawFixture is a service with one paired device, widening_change
// withdrawable, an effect that counts its runs, and an observer that records
// terminal statuses.
type withdrawFixture struct {
	*fixture
	d        *Device
	applied  int
	terminal []string
}

func newWithdrawFixture(t *testing.T) *withdrawFixture {
	f := &withdrawFixture{fixture: newFixture(t)}
	f.d, _ = f.firstDevice(t)
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error {
		f.applied++
		return nil
	})
	f.svc.RegisterWithdrawable(persistence.ApprovalKindWideningChange)
	f.svc.RegisterTerminalObserver(persistence.ApprovalKindWideningChange, func(_ context.Context, r persistence.AgentApprovalRequestRow, status string) {
		f.terminal = append(f.terminal, r.ID+":"+status)
	})
	return f
}

func (f *withdrawFixture) file(t *testing.T, id, kind string) {
	t.Helper()
	if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{
		ID: id, Namespace: "hermes", Kind: kind, Sentence: "Raise the limit.", Rendered: []byte(`{}`), RenderedSHA256: "h-" + id,
		Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL),
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *withdrawFixture) status(t *testing.T, id string) *persistence.AgentApprovalRequestRow {
	t.Helper()
	r, err := f.repo.GetRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestWithdraw_EndsAPendingRequestOnceAndIsIdempotent(t *testing.T) {
	f := newWithdrawFixture(t)
	ctx := context.Background()
	f.file(t, "apr_w1", persistence.ApprovalKindWideningChange)
	res, err := f.svc.Withdraw(ctx, "apr_w1", wBy, wReason)
	if err != nil || res.Outcome != WithdrawDone {
		t.Fatalf("withdraw = %+v, %v", res, err)
	}
	r := f.status(t, "apr_w1")
	if r.Status != persistence.ApprovalWithdrawn || r.WithdrawnBy != wBy || r.WithdrawReason != wReason || r.DecidedAt == nil {
		t.Fatalf("row = %+v", r)
	}
	res, err = f.svc.Withdraw(ctx, "apr_w1", wBy, wReason)
	if err != nil || res.Outcome != WithdrawAlready {
		t.Fatalf("second withdraw = %+v, %v", res, err)
	}
	if strings.Join(f.terminal, ",") != "apr_w1:withdrawn" {
		t.Fatalf("terminal observers = %v, want one withdrawn", f.terminal)
	}
	// A device decision after it is refused cleanly, and nothing applies.
	if err := f.svc.Decide(ctx, f.d, "apr_w1", "h-apr_w1", true); !errors.Is(err, ErrNotDecidable) {
		t.Fatalf("decide on a withdrawn request: %v", err)
	}
	if f.applied != 0 {
		t.Fatalf("the effect ran %d times on a withdrawn request", f.applied)
	}
	if f.countPushesContaining("apr_w1") != 0 {
		t.Fatal("a withdrawal pushed to the operator")
	}
}

func TestWithdraw_RefusesWhatItMayNotWithdraw(t *testing.T) {
	f := newWithdrawFixture(t)
	ctx := context.Background()
	f.file(t, "apr_host", persistence.ApprovalKindHostAction)
	f.file(t, "apr_ok", persistence.ApprovalKindWideningChange)
	if _, err := f.svc.Withdraw(ctx, "apr_host", wBy, wReason); !errors.Is(err, ErrNotWithdrawable) {
		t.Fatalf("an unregistered kind: %v", err)
	}
	if _, err := f.svc.Withdraw(ctx, "apr_nope", wBy, wReason); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}
	if _, err := f.svc.Withdraw(ctx, "apr_ok", "", wReason); !errors.Is(err, persistence.ErrBadWithdrawal) {
		t.Fatalf("no actor: %v", err)
	}
	if _, err := f.svc.Withdraw(ctx, "apr_ok", wBy, "two\nlines"); !errors.Is(err, persistence.ErrBadWithdrawal) {
		t.Fatalf("a control character: %v", err)
	}
	for _, id := range []string{"apr_host", "apr_ok"} {
		if s := f.status(t, id).Status; s != persistence.ApprovalPending {
			t.Fatalf("%s is %s after a refused withdrawal", id, s)
		}
	}
}

// The seam forces a withdrawal between Decide's read of the row and its
// guarded statement: the decision is refused and no effect runs.
func TestWithdraw_WinsARaceWithADecision(t *testing.T) {
	f := newWithdrawFixture(t)
	ctx := context.Background()
	f.file(t, "apr_r1", persistence.ApprovalKindWideningChange)
	fired := false
	f.svc.beforeTransition = func(op, id string) {
		if op == "decide" && !fired {
			fired = true
			if res, err := f.svc.Withdraw(ctx, id, wBy, wReason); err != nil || res.Outcome != WithdrawDone {
				t.Errorf("withdraw inside the decision = %+v, %v", res, err)
			}
		}
	}
	if err := f.svc.Decide(ctx, f.d, "apr_r1", "h-apr_r1", true); !errors.Is(err, ErrNotDecidable) {
		t.Fatalf("decide that lost the race: %v", err)
	}
	if !fired || f.applied != 0 || f.status(t, "apr_r1").Status != persistence.ApprovalWithdrawn {
		t.Fatalf("fired=%v applied=%d status=%s", fired, f.applied, f.status(t, "apr_r1").Status)
	}
}

// The seam forces a decision between Withdraw's read and its guarded
// statement: the withdrawal is too late, the decision stands and its effect
// ran exactly once.
func TestWithdraw_LosesARaceWithADecision(t *testing.T) {
	f := newWithdrawFixture(t)
	ctx := context.Background()
	f.file(t, "apr_r2", persistence.ApprovalKindWideningChange)
	fired := false
	f.svc.beforeTransition = func(op, id string) {
		if op == "withdraw" && !fired {
			fired = true
			if err := f.svc.Decide(ctx, f.d, id, "h-"+id, true); err != nil {
				t.Errorf("decide inside the withdrawal: %v", err)
			}
		}
	}
	res, err := f.svc.Withdraw(ctx, "apr_r2", wBy, wReason)
	if err != nil || res.Outcome != WithdrawTooLate || res.Status != persistence.ApprovalApproved {
		t.Fatalf("withdraw that lost the race = %+v, %v", res, err)
	}
	if !fired || f.applied != 1 || f.status(t, "apr_r2").Status != persistence.ApprovalApproved {
		t.Fatalf("fired=%v applied=%d status=%s", fired, f.applied, f.status(t, "apr_r2").Status)
	}
	if strings.Join(f.terminal, ",") != "apr_r2:approved" {
		t.Fatalf("terminal observers = %v", f.terminal)
	}
}

// The device page says the request was withdrawn and why; it offers no
// decision and never calls it approved, rejected, expired or decided. A
// decision posted from a page opened before is refused with a 409.
func TestWithdraw_DevicePageShowsTheWithdrawal(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	f.svc.RegisterWithdrawable(persistence.ApprovalKindWideningChange)
	fileReq(t, f, "apr_p1")
	if _, err := f.svc.Withdraw(context.Background(), "apr_p1", wBy, wReason); err != nil {
		t.Fatal(err)
	}
	res, body := phone.do(http.MethodGet, "/ui/approve/apr_p1", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Vornik withdrew this request") || !strings.Contains(body, "within the $8 limit") ||
		!strings.Contains(body, "Nothing needs deciding.") {
		t.Fatalf("page %d:\n%s", res.StatusCode, body)
	}
	for _, not := range []string{`name="decision"`, "This request expired", "Already decided", "Already answered"} {
		if strings.Contains(body, not) {
			t.Errorf("a withdrawn request's page carries %q", not)
		}
	}
	res, body = phone.do(http.MethodPost, "/ui/approve/apr_p1", url.Values{"decision": {"approve"}, "rendered_sha256": {"sha-apr_p1"}}, nil)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Vornik withdrew this request") {
		t.Fatalf("POST on a withdrawn request: %d\n%s", res.StatusCode, body)
	}
	if r, _ := f.repo.GetRequest(context.Background(), "apr_p1"); r.Status != persistence.ApprovalWithdrawn {
		t.Fatalf("a POST changed a withdrawn request: %s", r.Status)
	}
}

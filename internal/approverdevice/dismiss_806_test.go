package approverdevice

// GitHub #806 (design 22 §22.2.16d, part 06 §6.5 amendment 2026-11-17).
// Found at: production (operator: a failed apply and a withheld-tools notice
// stayed on the phone with no action). Regression tests at: integration (the
// real phone handlers and templates over the SQLite repository); the console
// half is in internal/ui, with its Chromium case.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestPhone806_DismissHidesAFailedApply(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	failRequest669(t, f, "apr_d", "Connect the work mailbox.", "approverdevice: the change cannot be applied: the change is rejected")
	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if !strings.Contains(list, `action="/ui/approve/apr_d/dismiss"`) || !strings.Contains(list, "Ask your assistant to request it again.") {
		t.Fatalf("a permanent failure has no Dismiss or next step:\n%s", list)
	}
	if strings.Contains(list, `action="/ui/approve/apr_d/retry"`) {
		t.Fatal("a permanent failure offers Retry")
	}
	res, _ := phone.do(http.MethodPost, "/ui/approve/apr_d/dismiss", nil, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("dismiss answered %d", res.StatusCode)
	}
	if _, list = phone.do(http.MethodGet, "/ui/approve/", nil, nil); strings.Contains(list, "Connect the work mailbox.") {
		t.Fatal("the dismissed failure is still listed")
	}
	if rows, _, _ := f.svc.ListApplyFailed(context.Background()); len(rows) != 0 {
		t.Fatalf("the console's list still holds %d failed rows", len(rows))
	}
	r, _ := f.repo.GetRequest(context.Background(), "apr_d")
	if r.DismissedBy == "" || !strings.HasPrefix(r.DismissedBy, "device:") {
		t.Errorf("dismissed_by = %q, want the device", r.DismissedBy)
	}
	// A cross-origin POST is refused before anything changes.
	failRequest669(t, f, "apr_x", "Another change.", "busy")
	if res, _ := phone.do(http.MethodPost, "/ui/approve/apr_x/dismiss", nil, map[string]string{"Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin dismiss answered %d, want 403", res.StatusCode)
	}
}

func TestPhone806_RetryRerunsAGivenUpApply(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	calls := 0
	fail := true
	f.svc.RegisterEffect(persistence.ApprovalKindDeviceEnrollment, func(context.Context, persistence.AgentApprovalRequestRow) error {
		calls++
		if fail {
			return errors.New("busy")
		}
		return nil
	})
	long := GaveUpPrefix + "60 attempts: " + strings.Repeat("y", 400)
	failRequest669(t, f, "apr_r", "Add the calendar.", long)
	failRequest669(t, f, "apr_p", "Add the mailbox.", "approverdevice: the change cannot be applied: x")
	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if !strings.Contains(list, `action="/ui/approve/apr_r/retry"`) {
		t.Fatalf("a given-up failure with a reason over 200 runes offers no Retry:\n%s", list)
	}
	// A retry that fails transiently again leaves the request for the tick.
	if res, _ := phone.do(http.MethodPost, "/ui/approve/apr_r/retry", nil, nil); res.StatusCode != http.StatusSeeOther || calls != 1 {
		t.Fatalf("retry answered %d after %d effect runs", res.StatusCode, calls)
	}
	if rows, _ := f.repo.ListApprovedUnapplied(context.Background()); len(rows) != 1 || rows[0].ID != "apr_r" {
		t.Fatalf("after a failed retry the request is not back for the tick: %v", rows)
	}
	fail = false
	f.advance(applyLease + 1)
	if err := f.svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := f.repo.GetRequest(context.Background(), "apr_r")
	if r.AppliedAt == nil || r.ApplyError != "" || !strings.HasPrefix(r.RetriedBy, "device:") {
		t.Fatalf("the retried request did not apply: applied %v error %q retried_by %q", r.AppliedAt, r.ApplyError, r.RetriedBy)
	}
	// A permanent failure is refused retry.
	if res, body := phone.do(http.MethodPost, "/ui/approve/apr_p/retry", nil, nil); res.StatusCode != http.StatusConflict || !strings.Contains(body, "request it again") {
		t.Errorf("retry of a permanent failure answered %d", res.StatusCode)
	}
}

func TestPhone806_AcknowledgeANotice(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	live := []Notice{{Key: "k-mail", Text: "Withheld: hermes's server mail in comms."}}
	f.svc.SetWithheldTools(func(context.Context) []Notice { return live })
	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if !strings.Contains(list, `action="/ui/approve/notices/k-mail/ack"`) {
		t.Fatalf("the notice has no Acknowledge:\n%s", list)
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/notices/k-mail/ack", nil, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("ack answered %d", res.StatusCode)
	}
	if _, list = phone.do(http.MethodGet, "/ui/approve/", nil, nil); strings.Contains(list, "server mail in comms") {
		t.Fatal("the acknowledged notice is still shown")
	}
	// A notice for one more tool has a new key and is shown.
	live = append(live, Notice{Key: "k-mail-2", Text: "Withheld: hermes's server mail in comms, one more tool."})
	if got := f.svc.Notices(context.Background()); len(got) != 1 || got[0].Key != "k-mail-2" {
		t.Fatalf("notices = %v, want only the new one", got)
	}
	// The suspension ends, so the ack is pruned; a recurrence is shown again.
	live = nil
	_ = f.svc.Notices(context.Background())
	live = []Notice{{Key: "k-mail", Text: "Withheld: hermes's server mail in comms."}}
	if got := f.svc.Notices(context.Background()); len(got) != 1 || got[0].Key != "k-mail" {
		t.Fatalf("a recurrence after the suspension ended = %v, want it shown", got)
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/notices/no-such/ack", nil, nil); res.StatusCode != http.StatusConflict {
		t.Errorf("ack of an unknown notice answered %d, want 409", res.StatusCode)
	}
}

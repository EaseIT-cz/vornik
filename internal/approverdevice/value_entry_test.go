package approverdevice

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

const canaryValue = "CANARY-sk-live-0123456789abcdef"

// Design §8.2, plan P4.2: a credential slot's page takes the value, and
// entering it is the approval. The page hands the value to the registered
// entry (which decides, then stores) and never renders it back. Control:
// the value-entry branch of decide.
func TestPages_CredentialSlotTakesAValue(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	var got []string
	storeFails := false
	f.svc.RegisterEffect(persistence.ApprovalKindCredentialSlot, func(context.Context, persistence.AgentApprovalRequestRow) error { return nil })
	f.svc.RegisterValueEntry(persistence.ApprovalKindCredentialSlot, func(ctx context.Context, d *Device, r persistence.AgentApprovalRequestRow, sha string, value []byte) error {
		if err := f.svc.Decide(ctx, d, r.ID, sha, true); err != nil {
			return err
		}
		got = append(got, string(value))
		if storeFails {
			return ErrValueNotStored
		}
		return nil
	})
	file := func(id string) {
		if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: id, Kind: persistence.ApprovalKindCredentialSlot,
			Sentence: "Your assistant asks you to add the credential FIO.", Rendered: []byte(`{"a":1}`), RenderedSHA256: "hhh",
			Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
			t.Fatal(err)
		}
	}
	file("apr_slot")
	_, page := phone.do(http.MethodGet, "/ui/approve/apr_slot", nil, nil)
	if !strings.Contains(page, `type="password"`) || !strings.Contains(page, `name="value"`) || !strings.Contains(page, `autocomplete="off"`) {
		t.Fatalf("the slot page has no value field: %s", page)
	}
	res, body := phone.do(http.MethodPost, "/ui/approve/apr_slot", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}, "value": {""}}, nil)
	if res.StatusCode != http.StatusBadRequest || len(got) != 0 {
		t.Fatalf("an empty value: %d, entry calls %d", res.StatusCode, len(got))
	}
	if r, _ := f.repo.GetRequest(context.Background(), "apr_slot"); r.Status != persistence.ApprovalPending {
		t.Fatalf("an empty value decided the request: %s", r.Status)
	}
	_ = body
	res, body = phone.do(http.MethodPost, "/ui/approve/apr_slot", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}, "value": {canaryValue}}, nil)
	if res.StatusCode != http.StatusSeeOther || len(got) != 1 || got[0] != canaryValue {
		t.Fatalf("approve with a value: %d, entry saw %q", res.StatusCode, got)
	}
	if strings.Contains(body, canaryValue) || strings.Contains(res.Header.Get("Location"), canaryValue) {
		t.Fatal("the value was echoed")
	}
	_, after := phone.do(http.MethodGet, "/ui/approve/apr_slot", nil, nil)
	if strings.Contains(after, canaryValue) {
		t.Fatal("the decided page shows the value")
	}

	// The store failed after the decision: the page says so, without the value.
	file("apr_slot2")
	storeFails = true
	res, body = phone.do(http.MethodPost, "/ui/approve/apr_slot2", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}, "value": {canaryValue}}, nil)
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "could not be stored") || strings.Contains(body, canaryValue) {
		t.Fatalf("store failure: %d %s", res.StatusCode, body)
	}

	// An oversized value is refused before anything is decided.
	file("apr_slot3")
	big := strings.Repeat("x", 17<<10)
	res, _ = phone.do(http.MethodPost, "/ui/approve/apr_slot3", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}, "value": {big}}, nil)
	if res.StatusCode < 400 {
		t.Fatalf("an oversized value was accepted: %d", res.StatusCode)
	}
	if r, _ := f.repo.GetRequest(context.Background(), "apr_slot3"); r.Status != persistence.ApprovalPending {
		t.Fatalf("an oversized value decided the request: %s", r.Status)
	}

	// Reject needs no value and stores nothing.
	file("apr_slot4")
	calls := len(got)
	if res, _ := phone.do(http.MethodPost, "/ui/approve/apr_slot4", url.Values{"decision": {"reject"}, "rendered_sha256": {"hhh"}}, nil); res.StatusCode != http.StatusSeeOther || len(got) != calls {
		t.Fatalf("reject: %d, entry calls %d", res.StatusCode, len(got)-calls)
	}
}

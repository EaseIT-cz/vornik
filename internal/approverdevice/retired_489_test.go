package approverdevice

// GitHub #489 (design part 09 §9.2e): the phone's Devices page listed
// revoked devices forever, and its Standing approvals page listed ended
// grants after the live ones. Both now show live entries by default
// (?status=all shows the rest), and a revoked device can be removed, with an
// audit row that keeps its label. Found at: production (operator,
// 2026-10-21). Regression test at: integration (the real device pages over
// SQLite).

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type audit489 struct {
	rows []*persistence.AdminAuditEntry
}

func (a *audit489) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	a.rows = append(a.rows, e)
	return nil
}
func (a *audit489) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

// plantDevice adds a further device the way a completed enrollment does,
// revoked when asked. The first device must exist already.
func plantDevice(t *testing.T, f *fixture, id, label string, revoked bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.repo.CreatePairing(ctx, persistence.ApproverPairingRow{CodeHash: "code-" + id, Label: label, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(10 * 60e9)}); err != nil {
		t.Fatal(err)
	}
	d := persistence.ApproverDeviceRow{ID: id, Label: label, TokenHash: "tok-" + id, PairedAt: f.clock(), PairedBy: "device:x", LastUsedAt: f.clock()}
	req := persistence.AgentApprovalRequestRow{ID: "apr_" + id, Kind: persistence.ApprovalKindDeviceEnrollment, Sentence: "s", Rendered: []byte(`{}`),
		RenderedSHA256: "sha-" + id, Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(3600e9)}
	if _, err := f.repo.RedeemPairing(ctx, "code-"+id, "claim-"+id, d, req, f.clock()); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.CompletePairing(ctx, "claim-"+id, d); err != nil {
		t.Fatal(err)
	}
	if revoked {
		if err := f.repo.RevokeDevice(ctx, id, f.clock()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDevices489_RevokedDevicesAreHiddenUnlessAll(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	plantDevice(t, f, "dev_live2", "Tablet", false)
	for _, id := range []string{"dev_old1", "dev_old2", "dev_old3"} {
		plantDevice(t, f, id, "Old "+id, true)
	}
	_, body := phone.do(http.MethodGet, "/ui/approve/devices", nil, nil)
	if !strings.Contains(body, "Tablet") || strings.Contains(body, "Old dev_old1") {
		t.Errorf("the default list should show live devices only:\n%s", body)
	}
	if !strings.Contains(body, "3 revoked devices are hidden.") || !strings.Contains(body, `class="act" href="/ui/approve/devices?status=all"`) {
		t.Errorf("the default list should say what it hid and link to the rest:\n%s", body)
	}
	_, all := phone.do(http.MethodGet, "/ui/approve/devices?status=all", nil, nil)
	if strings.Count(all, "Old dev_old") != 3 || strings.Count(all, "/remove\"") != 3 {
		t.Errorf("?status=all should list the 3 revoked devices, each with Remove:\n%s", all)
	}
	if strings.Contains(all, "dev_live2/remove") {
		t.Error("a live device offers Remove")
	}
}

func TestDevices489_RemoveARevokedDevice(t *testing.T) {
	f := newFixture(t)
	audit := &audit489{}
	f.svc.SetAudit(audit)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	plantDevice(t, f, "dev_old", "Old phone", true)
	plantDevice(t, f, "dev_live2", "Tablet", false)
	me, _ := f.svc.Authenticate(context.Background(), phone.deviceCookie())

	res, _ := phone.do(http.MethodPost, "/ui/approve/devices/dev_old/remove", url.Values{}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove a revoked device: %d", res.StatusCode)
	}
	devs, _ := f.svc.ListDevices(context.Background())
	for _, d := range devs {
		if d.ID == "dev_old" {
			t.Fatal("the removed device is still stored")
		}
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "device.remove" || audit.rows[0].Target != "dev_old" ||
		audit.rows[0].Principal != "approver_device:"+me.ID || !strings.Contains(audit.rows[0].After, "Old phone") {
		t.Fatalf("want one device.remove row by this device keeping the label, got %+v", audit.rows)
	}

	res, body := phone.do(http.MethodPost, "/ui/approve/devices/dev_live2/remove", url.Values{}, nil)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Revoke this device first") {
		t.Errorf("remove a live device: %d, want 409 and the reason", res.StatusCode)
	}
	if len(audit.rows) != 1 {
		t.Error("a refused remove wrote an audit row")
	}
	// Cross-origin, like revoke, is refused before the handler.
	res, _ = phone.do(http.MethodPost, "/ui/approve/devices/dev_live2/remove", url.Values{}, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": ""})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-site remove: %d, want 403", res.StatusCode)
	}
}

func TestDevices489_RemoveWithoutAnAuditWriterIsRefused(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	plantDevice(t, f, "dev_old", "Old phone", true)
	res, _ := phone.do(http.MethodPost, "/ui/approve/devices/dev_old/remove", url.Values{}, nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("remove with no audit writer: %d, want 500", res.StatusCode)
	}
	found := false
	devs, _ := f.svc.ListDevices(context.Background())
	for _, d := range devs {
		found = found || d.ID == "dev_old"
	}
	if !found {
		t.Fatal("a device was removed with no audit record")
	}
}

type standing489 struct{ views []StandingView }

func (s standing489) List(context.Context) ([]StandingView, error) { return s.views, nil }
func (standing489) Change(context.Context, string, string) error   { return nil }
func (standing489) Reaffirm(context.Context, string, string) error { return nil }

func TestStanding489_EndedGrantsAreHiddenUnlessAll(t *testing.T) {
	f := newFixture(t)
	f.svc.SetStandingPages(standing489{views: []StandingView{
		{ID: "bsg_a", Workflow: "live-flow", State: "active"},
		{ID: "bsg_p", Workflow: "paused-flow", State: "paused"},
		{ID: "bsg_s", Workflow: "suspended-flow", State: "suspended"},
		{ID: "bsg_r", Workflow: "revoked-flow", State: "revoked"},
		{ID: "bsg_e", Workflow: "expired-flow", State: "expired"},
		{ID: "bsg_u", Workflow: "spent-flow", State: "used"},
	}})
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	_, body := phone.do(http.MethodGet, "/ui/approve/standing", nil, nil)
	for _, want := range []string{"live-flow", "paused-flow", "suspended-flow", "3 ended grants are hidden.", `class="act" href="/ui/approve/standing?status=all"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the default Standing page lacks %q", want)
		}
	}
	for _, gone := range []string{"revoked-flow", "expired-flow", "spent-flow"} {
		if strings.Contains(body, gone) {
			t.Errorf("the default Standing page shows the ended %s", gone)
		}
	}
	_, all := phone.do(http.MethodGet, "/ui/approve/standing?status=all", nil, nil)
	for _, want := range []string{"revoked-flow", "expired-flow", "spent-flow", "live-flow"} {
		if !strings.Contains(all, want) {
			t.Errorf("?status=all lacks %q", want)
		}
	}
}

package ui

// GitHub #763 (agent-administered design part 09, amendment #763): the
// console lists, pairs, revokes and removes approver devices, for an
// owner-class browser session only. An API key of any class is refused, so
// an assistant holding the admin key `vornikctl connect` mints cannot pair
// here. A tenant_admin pairs only a further device, and may not revoke the
// last live one. Found at: n/a (feature). Tests at: unit, through the
// console's handler and the real auth middleware.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

type fakeApprovers763 struct {
	devices []persistence.ApproverDeviceRow
	push    bool
	minted  []string // label|principal
	revoked []string
	removed []string
	// unrecorded makes RevokeBy revoke and report the audit row unwritten.
	unrecorded bool
}

func (f *fakeApprovers763) StartPairingBy(_ context.Context, label, principal, _ string) (string, time.Time, error) {
	f.minted = append(f.minted, label+"|"+principal)
	return "ABCDEFGH", time.Now().Add(10 * time.Minute), nil
}
func (f *fakeApprovers763) DeviceExists(context.Context) (bool, error) {
	return f.live() > 0, nil
}
func (f *fakeApprovers763) live() int {
	n := 0
	for _, d := range f.devices {
		if d.RevokedAt == nil {
			n++
		}
	}
	return n
}
func (f *fakeApprovers763) ListDevices(context.Context) ([]persistence.ApproverDeviceRow, error) {
	return f.devices, nil
}
func (f *fakeApprovers763) RevokeBy(_ context.Context, id, _, _ string) error {
	if f.unrecorded {
		f.revoked = append(f.revoked, id)
		return approverdevice.ErrNoAudit
	}
	for i := range f.devices {
		if f.devices[i].ID == id {
			if f.devices[i].RevokedAt != nil {
				return approverdevice.ErrAlreadyRevoked
			}
			now := time.Now()
			f.devices[i].RevokedAt = &now
			f.revoked = append(f.revoked, id)
			return nil
		}
	}
	return persistence.ErrNotFound
}
func (f *fakeApprovers763) RevokeKeepingOne(ctx context.Context, id, p, s string) error {
	if f.live() <= 1 {
		for _, d := range f.devices {
			if d.ID == id && d.RevokedAt == nil {
				return approverdevice.ErrLastDevice
			}
		}
	}
	return f.RevokeBy(ctx, id, p, s)
}
func (f *fakeApprovers763) Remove(_ context.Context, id, _, _ string) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeApprovers763) PushConfigured() bool { return f.push }

const dev763 = "dev_0123456789abcdef"

func approversServer763(devices int, opts ...ServerOption) (*Server, *fakeApprovers763) {
	f := &fakeApprovers763{push: true}
	for i := 0; i < devices; i++ {
		id := dev763
		if i > 0 {
			id = "dev_fedcba987654321" + string(rune('0'+i))
		}
		f.devices = append(f.devices, persistence.ApproverDeviceRow{ID: id, Label: "Pixel", PairedAt: time.Now().Add(-time.Hour), LastUsedAt: time.Now()})
	}
	return NewServer(append([]ServerOption{WithApproverDevices(f)}, opts...)...), f
}

func post763(path, role string) *http.Request {
	req := setupAuthRequest(http.MethodPost, path, role)
	req.Body = http.NoBody
	req.Form, req.PostForm = url.Values{"label": {"My phone"}}, url.Values{"label": {"My phone"}}
	return req
}

func serve763(s *Server, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func TestApprovers763_AdminListsPairsRevokesRemoves(t *testing.T) {
	var logs bytes.Buffer
	s, f := approversServer763(0, WithLogger(zerolog.New(&logs)))
	if rec := serve763(s, setupAuthRequest(http.MethodGet, "/settings/approvers", auth.RoleAdmin)); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "ABCD-EFGH") {
		t.Fatalf("list: %d", rec.Code)
	}
	rec := serve763(s, post763("/settings/approvers/pair", auth.RoleAdmin)) // the first device
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ABCD-EFGH") || !strings.Contains(rec.Body.String(), "/ui/pair") {
		t.Fatalf("pair: %d %q", rec.Code, rec.Body.String())
	}
	if len(f.minted) != 1 || !strings.HasPrefix(f.minted[0], "My phone|") {
		t.Fatalf("minted %v", f.minted)
	}
	if strings.Contains(logs.String(), "ABCDEFGH") || strings.Contains(logs.String(), "ABCD-EFGH") {
		t.Fatal("the pairing code reached a log line")
	}

	s, f = approversServer763(2)
	if rec := serve763(s, post763("/settings/approvers/"+dev763+"/revoke", auth.RoleAdmin)); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/ui/settings/approvers?notice=Revoked" || len(f.revoked) != 1 {
		t.Fatalf("revoke: %d to %q, %v", rec.Code, rec.Header().Get("Location"), f.revoked)
	}
	if rec := serve763(s, post763("/settings/approvers/"+dev763+"/revoke", auth.RoleAdmin)); rec.Code != http.StatusConflict {
		t.Fatalf("revoke again: %d, want 409", rec.Code)
	}
	if rec := serve763(s, post763("/settings/approvers/dev_nope/revoke", auth.RoleAdmin)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d, want 404", rec.Code)
	}
	if rec := serve763(s, post763("/settings/approvers/"+dev763+"/remove", auth.RoleAdmin)); rec.Code >= 400 || len(f.removed) != 1 {
		t.Fatalf("remove: %d %v", rec.Code, f.removed)
	}
}

func TestApprovers763_TenantAdmin(t *testing.T) {
	s, f := approversServer763(0)
	rec := serve763(s, post763("/settings/approvers/pair", auth.RoleTenantAdmin))
	if rec.Code != http.StatusForbidden || len(f.minted) != 0 || !strings.Contains(rec.Body.String(), "out of band") {
		t.Fatalf("tenant_admin first device: %d minted %v body %q", rec.Code, f.minted, rec.Body.String())
	}
	s, f = approversServer763(1)
	if rec := serve763(s, post763("/settings/approvers/pair", auth.RoleTenantAdmin)); rec.Code != http.StatusOK || len(f.minted) != 1 {
		t.Fatalf("tenant_admin further device: %d minted %v", rec.Code, f.minted)
	}
	if rec := serve763(s, setupAuthRequest(http.MethodGet, "/settings/approvers", auth.RoleTenantAdmin)); rec.Code != http.StatusOK {
		t.Fatalf("tenant_admin list: %d", rec.Code)
	}
	// The last live device: refused, so a stolen session cannot empty the
	// table and turn a further-device code into a first-device one.
	if rec := serve763(s, post763("/settings/approvers/"+dev763+"/revoke", auth.RoleTenantAdmin)); rec.Code != http.StatusConflict || len(f.revoked) != 0 {
		t.Fatalf("tenant_admin last device: %d %v", rec.Code, f.revoked)
	}
	s, f = approversServer763(2)
	if rec := serve763(s, post763("/settings/approvers/"+dev763+"/revoke", auth.RoleTenantAdmin)); rec.Code >= 400 || len(f.revoked) != 1 {
		t.Fatalf("tenant_admin revoke with another live device: %d %v", rec.Code, f.revoked)
	}
}

func TestApprovers763_WarnsWithoutAnAlertChannel(t *testing.T) {
	s, f := approversServer763(0)
	f.push = false
	rec := serve763(s, post763("/settings/approvers/pair", auth.RoleAdmin))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-no-alert") {
		t.Fatalf("no alert channel: %d, warning shown %v", rec.Code, strings.Contains(rec.Body.String(), "data-no-alert"))
	}
}

func TestApprovers763_NoUserNoKeyNoAuthOff(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodGet, "/settings/approvers"},
		{http.MethodPost, "/settings/approvers/pair"},
		{http.MethodPost, "/settings/approvers/" + dev763 + "/revoke"},
		{http.MethodPost, "/settings/approvers/" + dev763 + "/remove"},
	}
	for _, rt := range routes {
		s, f := approversServer763(2)
		if rec := serve763(s, setupAuthRequest(rt.method, rt.path, "user")); rec.Code != http.StatusForbidden {
			t.Errorf("user session %s %s: %d", rt.method, rt.path, rec.Code)
		}
		if rec := serve763(s, apiKeyReq763(rt.method, rt.path)); rec.Code != http.StatusForbidden {
			t.Errorf("admin-class API key %s %s: %d", rt.method, rt.path, rec.Code)
		}
		if rec := serve763(s, httptest.NewRequest(rt.method, rt.path, nil)); rec.Code != http.StatusForbidden {
			t.Errorf("auth off %s %s: %d", rt.method, rt.path, rec.Code)
		}
		if len(f.minted)+len(f.revoked)+len(f.removed) != 0 {
			t.Errorf("%s %s: a refused caller changed devices", rt.method, rt.path)
		}
	}
}

// apiKeyReq763 is a request authenticated by an all-projects (admin-class)
// static API key through the real auth middleware: no session.
func apiKeyReq763(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-API-Key", "sk-test-763")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{Enabled: true, StaticAPIKeys: map[string][]string{"sk-test-763": {}}})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { captured = r })).ServeHTTP(httptest.NewRecorder(), req)
	if captured == nil {
		panic("the key was not admitted")
	}
	return captured
}

func TestApprovers763_UnrecordedRevokeIsSaid(t *testing.T) {
	s, f := approversServer763(2)
	f.unrecorded = true
	rec := serve763(s, post763("/settings/approvers/"+dev763+"/revoke", auth.RoleAdmin))
	if rec.Code != http.StatusSeeOther || len(f.revoked) != 1 ||
		rec.Header().Get("Location") != "/ui/settings/approvers?notice="+url.QueryEscape("Revoked, but the admin audit could not record it") {
		t.Fatalf("unrecorded revoke: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPlainHTTPNonLoopbackOrigin763(t *testing.T) {
	for origin, want := range map[string]bool{
		"http://vornik.lan:8080": true, "https://vornik.lan": false,
		"http://localhost:8080": false, "http://127.0.0.1": false, "http://[::1]:8080": false,
	} {
		if got := plainHTTPNonLoopbackOrigin(origin); got != want {
			t.Errorf("%s: %v, want %v", origin, got, want)
		}
	}
}

// GitHub #833 (design §21): the page has its own Manage entry, so the panel
// marks "Approver phones" as the current page, not My account.
func TestApprovers833_MarksItsOwnNavEntry(t *testing.T) {
	s, _ := approversServer763(1)
	rec := serve763(s, setupAuthRequest(http.MethodGet, "/settings/approvers", auth.RoleAdmin))
	body := rec.Body.String()
	own := regexp.MustCompile(`href="/ui/settings/approvers"[^>]*panel-item-active`)
	mine := regexp.MustCompile(`href="/ui/account"[^>]*panel-item-active`)
	if rec.Code != http.StatusOK || !own.MatchString(body) || mine.MatchString(body) {
		t.Fatalf("%d: approvers entry active %v, My account active %v", rec.Code, own.MatchString(body), mine.MatchString(body))
	}
}

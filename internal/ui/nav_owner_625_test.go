package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
)

// GitHub #625 item 1 (18-security-and-tenancy.md, "Amendment 2026-11-12 (#625
// item 1)"): a managed instance's owner (tenant_admin) had no nav entry to the
// pages B3 grants them, because the whole Admin area answers only to the
// "admin" marker. The owner marker plus OwnerHref give the owner exactly the
// entries the server admits.
//
// Found at: review (#623's "Not in this change"); a user observes it at
// production. Regression test at: integration (the production UI handler for
// the gate agreement; the rendered nav partial for the markup).

// ownerEntries is the design's list: where each owner-visible entry goes.
var ownerEntries = map[string]string{
	// Since #755 the owner's entries live in Manage, which is not
	// admin-only, so the old Admin area's owner rail target is gone.
	"my-account": "/ui/account",
	"admin-keys": "/ui/credentials",
	// GitHub #789: the console pages the owner reads since #787.
	"operator":            "/ui/admin/",
	"admin":               "/ui/admin/",
	"admin-control-plane": "/ui/admin/control-plane",
	"admin-skills":        "/ui/admin/skills",
	"admin-agents":        "/ui/admin/agents",
	// GitHub #833 (design §21): the approver-devices page (#763).
	"approvers": "/ui/settings/approvers",
}

func TestNavOwner625_OwnerHrefExactlyOnTheDesignedEntries(t *testing.T) {
	got := map[string]string{}
	for _, a := range navModel() {
		if a.OwnerHref != "" {
			got[a.Key] = a.OwnerHref
		}
		for _, d := range a.Dests {
			if d.OwnerHref != "" {
				got[d.Key] = d.OwnerHref
			}
		}
	}
	if len(got) != len(ownerEntries) {
		t.Fatalf("owner entries = %v, want %v", got, ownerEntries)
	}
	for k, want := range ownerEntries {
		if got[k] != want {
			t.Errorf("OwnerHref[%s] = %q, want %q", k, got[k], want)
		}
	}
}

// Every owner target admits a tenant_admin session; every other entry of the
// Admin area is either under /ui/admin/ (refused to every non-admin by
// admin.Middleware) or listed here as not measured for the owner.
func TestNavOwner625_NavAgreesWithTheGates(t *testing.T) {
	srv, _, _, _ := hubRig623(t, nil) // a server with the credentials page wired
	WithSessionUserResolver(func(*http.Request) string { return "user_owner" })(srv)
	WithApproverDevices(&fakeApprovers763{push: true})(srv) // #833: the approvers page
	// The production admin gate in front (wrapUIAdminGate's owner-read hook,
	// GitHub #787), so a console entry is judged by the real decision (#789).
	gate := func(next http.Handler) http.Handler {
		return admin.Middleware(config.AdminConfig{Enabled: true, AllowedKeys: []string{"admin-key"}}, nil,
			func(*http.Request) bool { return true }, func(*http.Request) bool { return false }, "/admin",
			admin.WithOwnerRead(func(r *http.Request) bool { return api.OwnerConsoleRead(r, api.AdminRouterPath(r.URL.Path)) }),
		)(next)
	}
	h := gate(srv.Handler())
	console := gate(NewServer().Handler()) // the console pages, with no stub stores behind them
	for key, href := range ownerEntries {
		rec := httptest.NewRecorder()
		target := h
		if strings.HasPrefix(href, "/ui/admin/") {
			target = console
		}
		target.ServeHTTP(rec, setupAuthRequest(http.MethodGet, strings.TrimPrefix(href, "/ui"), auth.RoleTenantAdmin))
		if rec.Code != http.StatusOK {
			t.Errorf("owner entry %s -> %s answered %d to tenant_admin, want 200", key, href, rec.Code)
		}
	}
	notMeasured := map[string]bool{"integrations": true}
	for _, a := range navModel() {
		if !a.AdminOnly {
			continue
		}
		for _, d := range a.Dests {
			if d.OwnerHref != "" || d.AdminOnly || notMeasured[d.Key] {
				continue
			}
			if !strings.HasPrefix(d.Href, "/ui/admin/") {
				t.Errorf("Admin entry %s (%s) is hidden from the owner but is not an admin-only /ui/admin/ route", d.Key, d.Href)
			}
		}
	}
}

func TestNavOwner625_RenderedMarkup(t *testing.T) {
	srv := NewServer()
	var buf strings.Builder
	if err := srv.templates.ExecuteTemplate(&buf, "nav", map[string]any{"CurrentPage": "admin-keys"}); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	// Panel + drawer for Keys & access (#755: no owner rail icon any more).
	if n := strings.Count(body, `data-owner-href="/ui/credentials"`); n != 2 {
		t.Errorf(`data-owner-href="/ui/credentials" = %d, want 2 (panel and drawer Keys & access)`, n)
	}
	if n := strings.Count(body, `data-owner-href="/ui/account"`); n != 2 {
		t.Errorf(`data-owner-href="/ui/account" = %d, want 2 (panel, drawer)`, n)
	}
	// Since #789 the operator console's non-admin-only entries (Admin
	// console, Control plane) point the owner at the readable pages, so no
	// entry is hidden from the owner by data-owner-hide any more.
	if n := strings.Count(body, "data-owner-hide "); n != 0 {
		t.Errorf("data-owner-hide = %d, want 0", n)
	}
	if n := strings.Count(body, `data-owner-href="/ui/admin/control-plane"`); n != 2 {
		t.Errorf(`data-owner-href="/ui/admin/control-plane" = %d, want 2 (panel, drawer)`, n)
	}
	if !strings.Contains(body, "vornik_session_ui=(1|admin|owner)") {
		t.Error("the marker script does not accept the owner marker")
	}
}

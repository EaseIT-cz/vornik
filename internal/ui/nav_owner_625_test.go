package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/auth"
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
	h := srv.Handler()
	for key, href := range ownerEntries {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, setupAuthRequest(http.MethodGet, strings.TrimPrefix(href, "/ui"), auth.RoleTenantAdmin))
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
	// Admin console and Control plane, the operator console's non-admin-only
	// entries: panel + drawer. Integrations, Skills and Coding agents are
	// AdminOnly in Manage since #755, hidden from the owner by that flag.
	if n := strings.Count(body, "data-owner-hide "); n != 4 {
		t.Errorf("data-owner-hide = %d, want 4 (2 entries x panel and drawer)", n)
	}
	if !strings.Contains(body, "vornik_session_ui=(1|admin|owner)") {
		t.Error("the marker script does not accept the owner marker")
	}
}

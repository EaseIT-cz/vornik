package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/listener"
	"vornik.io/vornik/internal/onboarding"
)

// GitHub #560 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1b)", B2b): the console's "restricts" sites admit any role but user,
// so managed hosting's instance owner (tenant_admin) is placed at each:
// refused on setup and on the daemon-global authoring roots, except project
// creation, which B3 grants. Found at: design review (review-20261028-f715 F2).

func TestTenantAdmin560_SetupRefused(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, setupAuthRequest(http.MethodGet, "/setup", auth.RoleTenantAdmin))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tenant_admin on /ui/setup: %d, want 403", rec.Code)
	}
}

func TestTenantAdmin560_GlobalAuthoringRefusedExceptProjectCreation(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: &config.Config{}}))
	for _, p := range []string{"/swarms", "/workflows/x", "/assistant/draft", "/audit", "/mcp"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, setupAuthRequest(http.MethodGet, p, auth.RoleTenantAdmin))
		if rec.Code != http.StatusForbidden {
			t.Errorf("tenant_admin on %s: %d, want 403", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, setupAuthRequest(http.MethodGet, "/projects/new", auth.RoleTenantAdmin))
	if rec.Code == http.StatusForbidden {
		t.Fatal("tenant_admin was refused /projects/new, which B3 grants")
	}
	if !tenantAdminAuthoringPath("/projects/new/wizard") || tenantAdminAuthoringPath("/projects/newer") {
		t.Fatal("tenantAdminAuthoringPath must match /projects/new and its subpaths only")
	}
}

// B2b: the owner gets the admin's technical story view, not the default
// viewer's narrated one (review 2fd9 F4).
func TestTenantAdmin560_StoryDefaultViewer(t *testing.T) {
	if isStoryDefaultViewer(setupAuthRequest(http.MethodGet, "/x", auth.RoleTenantAdmin)) {
		t.Fatal("tenant_admin treated as a story default viewer")
	}
	if !isStoryDefaultViewer(setupAuthRequest(http.MethodGet, "/x", auth.RoleUser)) {
		t.Fatal("a user is no longer a story default viewer")
	}
}

// M1c D3: in managed hosting the API-key login is not offered off the
// operations listener (404, no link); on the operations listener and
// self-hosted it is (401 challenge, link shown).
func TestKeyLogin560_NotOfferedOnPublicListener(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
		l       listener.Name
		offered bool
	}{
		{"managed public", true, listener.Public, false},
		{"managed operations", true, listener.Operations, true},
		{"self-hosted", false, listener.Public, true},
	} {
		srv := NewServer(WithManagedHosting(tc.managed))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/login?method=key", nil)
		srv.Handler().ServeHTTP(rec, req.WithContext(listener.With(req.Context(), tc.l)))
		wantCode := http.StatusNotFound
		if tc.offered {
			wantCode = http.StatusUnauthorized
		}
		if rec.Code != wantCode {
			t.Errorf("%s: method=key answered %d, want %d", tc.name, rec.Code, wantCode)
		}
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/login", nil)
		srv.Handler().ServeHTTP(rec, req.WithContext(listener.With(req.Context(), tc.l)))
		if got := strings.Contains(rec.Body.String(), "/ui/login?method=key"); got != tc.offered {
			t.Errorf("%s: key-login link shown = %v, want %v", tc.name, got, tc.offered)
		}
	}
}

package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/listener"
	"vornik.io/vornik/internal/registry"
)

// GitHub #587 (operations-api-contract-design.md §7.2.3, §7.2.3a): the
// managed instance's companion page. Owner-only on the public listener; a
// GET-only page whose script calls the existing grant, list and revoke
// routes, so no key is ever in its HTML.

func companionPageRequest(role string, l listener.Name) *http.Request {
	req := setupAuthRequest(http.MethodGet, "/companion", role)
	return req.WithContext(listener.With(req.Context(), l))
}

func companionServer(managed, withProject bool) *Server {
	reg := registry.New()
	if withProject {
		registry.SeedForTest(reg, map[string]*registry.Project{"companion": {ID: "companion"}})
	}
	return NewServer(WithManagedHosting(managed), WithProjectRegistry(reg), WithWebUIBaseURL("https://acme.vornik.cloud"))
}

func TestCompanionPage587_Gate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
		role    string
		l       listener.Name
		want    int
	}{
		{"self-hosted", false, auth.RoleTenantAdmin, listener.Public, http.StatusNotFound},
		{"a user session", true, auth.RoleUser, listener.Public, http.StatusForbidden},
		{"an admin session", true, auth.RoleAdmin, listener.Public, http.StatusForbidden},
		{"the operations listener", true, auth.RoleTenantAdmin, listener.Operations, http.StatusForbidden},
		{"the owner", true, auth.RoleTenantAdmin, listener.Public, http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		companionServer(tc.managed, true).Handler().ServeHTTP(rec, companionPageRequest(tc.role, tc.l))
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

func TestCompanionPage587_OnlyGET(t *testing.T) {
	req := setupAuthRequest(http.MethodPost, "/companion", auth.RoleTenantAdmin)
	req = req.WithContext(listener.With(req.Context(), listener.Public))
	rec := httptest.NewRecorder()
	companionServer(true, true).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /companion: %d, want 405 (the page mints nothing itself)", rec.Code)
	}
}

func TestCompanionPage587_TheOwnersPage(t *testing.T) {
	rec := httptest.NewRecorder()
	companionServer(true, true).Handler().ServeHTTP(rec, companionPageRequest(auth.RoleTenantAdmin, listener.Public))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-vornik") {
		t.Fatal("the page's HTML holds a key")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control %q, want no-store", cc)
	}
	for _, want := range []string{
		`<script src="/ui/static/companion.js"></script>`,
		"claude plugin marketplace add EaseIT-cz/vornik",
		"claude plugin install vornik-companion@vornik",
		`data-vornik-url="https://acme.vornik.cloud"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
}

// The page's script calls the three existing routes with the fixed grant,
// and writes API values only through textContent.
func TestCompanionPage587_TheScript(t *testing.T) {
	b, err := staticFS.ReadFile("static/companion.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{
		"'/api/v1/companion/grant'",
		"'/api/v1/companion/keys?projectId=companion'",
		"'/api/v1/projects/companion/keys/'",
		"projectId: 'companion', clientKind: 'claude-code'",
		"budgetCapUsd: 25",
		"textContent",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("companion.js does not contain %q", want)
		}
	}
	for _, refused := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "skillAdmin", "localStorage", "console.log"} {
		if strings.Contains(js, refused) {
			t.Errorf("companion.js uses %q", refused)
		}
	}
}

func TestCompanionPage587_NoCompanionProject(t *testing.T) {
	rec := httptest.NewRecorder()
	companionServer(true, false).Handler().ServeHTTP(rec, companionPageRequest(auth.RoleTenantAdmin, listener.Public))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Engineering") {
		t.Error("the page does not say the Engineering template was not chosen")
	}
	if strings.Contains(body, `id="companion-create"`) {
		t.Error("the create button is shown with no companion project")
	}
}

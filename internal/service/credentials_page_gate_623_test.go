package service

// GitHub #623 (found while implementing, 2026-11-02): the credentials page was
// first mounted at /ui/admin/credentials, where production's wrapUIAdminGate
// (admin.Middleware over the whole /ui/admin prefix) answers a tenant_admin
// session 401 before any handler runs, so a managed instance's owner could
// never reach it. The console-handler tests had not passed through that
// wrapper. This test drives the real chain (api.AuthMiddleware ->
// wrapUIAdminGate -> the console) and pins both the moved path and the reason.
// Regression test at: integration. Design:
// 2026-10-02-agent-administered-vornik-design.md, Amendment 2026-11-02 (#623).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/ui"
)

type stubCreds623 struct{}

func (stubCreds623) Status(context.Context) (ui.CredentialsView, error) {
	return ui.CredentialsView{}, nil
}
func (stubCreds623) Put(context.Context, string, []byte, string) error { return nil }
func (stubCreds623) Delete(context.Context, string) error              { return nil }
func (stubCreds623) Refresh(context.Context) (ui.CredentialsRefresh, error) {
	return ui.CredentialsRefresh{}, nil
}

func credentialsChain623(t *testing.T, sb auth.Backend) http.Handler {
	t.Helper()
	adminCfg := config.AdminConfig{Enabled: true, AllowedKeys: []string{"admin-key"}}
	console := ui.NewServer(ui.WithOperatorCredentials(stubCreds623{}), ui.WithOperatorCapability(adminCfg))
	h := wrapUIAdminGate(zerolog.Nop(), adminCfg, console.Handler())
	return api.AuthMiddleware(api.AuthConfig{
		Enabled:        true,
		StaticAPIKeys:  map[string][]string{"admin-key": nil},
		SessionBackend: sb,
	})(h)
}

func TestCredentialsPage623_ReachableThroughTheProductionChain(t *testing.T) {
	owner := credentialsChain623(t, &gateStubSessionBackend{token: "c", role: auth.RoleTenantAdmin})
	if rec := gateGet(owner, "/ui/credentials", "", "c"); rec.Code != http.StatusOK {
		t.Fatalf("tenant_admin on /ui/credentials: %d %q", rec.Code, rec.Body.String())
	}
	// Why the page is not under /ui/admin: the wrapper refuses the owner there.
	if rec := gateGet(owner, "/ui/admin/credentials", "", "c"); rec.Code == http.StatusOK {
		t.Fatal("tenant_admin reached /ui/admin/*, which wrapUIAdminGate reserves for admins")
	}
	admin := credentialsChain623(t, nil)
	if rec := gateGet(admin, "/ui/credentials", "admin-key", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin key on /ui/credentials: %d", rec.Code)
	}
	user := credentialsChain623(t, &projectUser623{})
	if rec := gateGet(user, "/ui/credentials", "", "u"); rec.Code != http.StatusNotFound {
		t.Fatalf("project-scoped user on /ui/credentials: %d, want 404", rec.Code)
	}
}

type projectUser623 struct{}

func (projectUser623) Name() string { return "session" }
func (projectUser623) Authenticate(_ context.Context, cred auth.Credential) (*auth.Identity, error) {
	if cred.SessionToken != "u" {
		return nil, auth.ErrNoCredential
	}
	return &auth.Identity{Subject: "user:p", Projects: []string{"proj-a"},
		Extra: map[string]any{auth.ExtraSessionRole: auth.RoleUser, auth.ExtraSessionID: "s2", auth.ExtraSessionUserID: "p"}}, nil
}

// Code review F1 (review-20261102-ce24): a cross-site POST to the credentials
// page is refused by the same-origin gate in api.AuthMiddleware, which wraps
// every console route, this one included.
func TestCredentialsPage623_CrossSitePostRefused(t *testing.T) {
	owner := credentialsChain623(t, &gateStubSessionBackend{token: "c", role: auth.RoleTenantAdmin})
	for _, site := range []string{"cross-site", ""} {
		req := httptest.NewRequest(http.MethodPost, "/ui/credentials", strings.NewReader("action=save&name=X&value=v"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "c"})
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		} else {
			req.Header.Set("Origin", "https://evil.example")
		}
		rec := httptest.NewRecorder()
		owner.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CSRF") {
			t.Errorf("cross-site POST (%q): %d %q, want 403 CSRF", site, rec.Code, rec.Body.String())
		}
	}
}

package service

// GitHub #787 (18-security-and-tenancy.md, amendment 2026-11-15 (#787)).
// Found at: production (vornik-cloud: a managed instance's owner cannot open
// the operator console). Regression test at: integration, the production
// chain (api.AuthMiddleware -> wrapUIAdminGate -> the console's real router
// and pages) over a migrated SQLite store.

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/ui"
)

func ownerConsoleChain787(t *testing.T, sb auth.Backend) (http.Handler, *sql.DB) {
	t.Helper()
	db := sqlitetest.Memory(t)
	adminCfg := config.AdminConfig{Enabled: true, AllowedKeys: []string{"admin-key"}}
	console := ui.NewServer(
		ui.WithOperatorCapability(adminCfg),
		ui.WithAdminAuditRepository(sqlite.NewAdminAuditRepository(db.DB)),
		ui.WithAdminChatAuditRepository(sqlite.NewChatAuditRepository(db.DB)),
		ui.WithCrossProjectCallRepository(sqlite.NewCrossProjectCallRepository(db.DB)),
		ui.WithProposalStore(sqlite.NewProposalRepository(db.DB)),
		ui.WithSkillRepository(sqlite.NewSkillRepository(db.DB)),
	)
	h := wrapUIAdminGate(zerolog.Nop(), adminCfg, console.Handler())
	return api.AuthMiddleware(api.AuthConfig{
		Enabled:        true,
		StaticAPIKeys:  map[string][]string{"admin-key": nil},
		SessionBackend: sb,
	})(h), db.DB
}

// tableChecksums hashes every row of every table, so an UPDATE or an insert
// netted by a delete shows (review 8f97 F6).
func tableChecksums(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	_ = rows.Close()
	out := map[string]string{}
	for _, n := range names {
		r, err := db.Query(`SELECT * FROM "` + n + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		h := sha256.New()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = r.Scan(ptrs...)
			_, _ = fmt.Fprintf(h, "%v\n", vals)
		}
		_ = r.Close()
		out[n] = fmt.Sprintf("%x", h.Sum(nil))
	}
	return out
}

func ownerDo(h http.Handler, method, path, cookie, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "vornik_session", Value: cookie})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The pages this rig wires a store for render 200 to the owner.
var readable787 = []string{"/ui/admin/", "/ui/admin/audit", "/ui/admin/chat-audit", "/ui/admin/health/cluster",
	"/ui/admin/cpc", "/ui/admin/skills", "/ui/admin/control-plane", "/ui/admin/control-plane?section=proposals"}

// Every readable page and detail pattern, from the real tables (review b8ba
// F2): a POST to any of them is refused.
func allReadable787() []string {
	var out []string
	for p := range api.OwnerConsoleReadablePaths {
		out = append(out, "/ui/admin"+p)
	}
	for prefix, n := range api.OwnerConsoleDetailPatterns {
		out = append(out, "/ui/admin"+prefix+strings.TrimSuffix(strings.Repeat("x1/", n), "/"))
	}
	return out
}

// Every refused page from the real table, plus the MCP section and actions.
func allRefused787() []string {
	out := []string{"/ui/admin/control-plane?section=mcp", "/ui/admin/workflow-proposals/wpr_1/decide", "/ui/admin/cpc/x/cancel", "/ui/admin/users/u1"}
	for p := range api.OwnerConsoleRefusedPaths {
		out = append(out, "/ui/admin"+p)
	}
	return out
}

func TestOwnerConsoleRead787_ThroughTheProductionChain(t *testing.T) {
	owner, db := ownerConsoleChain787(t, &gateStubSessionBackend{token: "o", role: auth.RoleTenantAdmin})
	before := tableChecksums(t, db)
	for _, p := range readable787 {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			rec := ownerDo(owner, m, p, "o", "")
			if rec.Code != http.StatusOK {
				t.Errorf("owner %s %s: %d %.200q", m, p, rec.Code, rec.Body.String())
				continue
			}
			if m == http.MethodGet && !strings.Contains(rec.Body.String(), "data-console-readonly") {
				t.Errorf("owner GET %s: no read-only marker", p)
			}
		}
	}
	for _, p := range allReadable787() {
		if rec := ownerDo(owner, http.MethodPost, p, "o", ""); rec.Code < 400 {
			t.Errorf("owner POST %s: %d, want refused", p, rec.Code)
		}
	}
	for _, p := range allRefused787() {
		if rec := ownerDo(owner, http.MethodGet, p, "o", ""); rec.Code == http.StatusOK {
			t.Errorf("owner GET %s: 200, want refused", p)
		}
	}
	after := tableChecksums(t, db)
	for n, sum := range before {
		if after[n] != sum {
			t.Errorf("an owner GET changed table %s", n)
		}
	}

	// An admin is unchanged and sees no marker.
	admin, _ := ownerConsoleChain787(t, nil)
	rec := ownerDo(admin, http.MethodGet, "/ui/admin/audit", "", "admin-key")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "data-console-readonly") {
		t.Fatalf("admin GET /ui/admin/audit: %d, marker=%v", rec.Code, strings.Contains(rec.Body.String(), "data-console-readonly"))
	}
	// A project user is not admitted.
	user, _ := ownerConsoleChain787(t, &projectUser623{})
	if rec := ownerDo(user, http.MethodGet, "/ui/admin/audit", "u", ""); rec.Code == http.StatusOK {
		t.Fatal("a project user read the console")
	}
}

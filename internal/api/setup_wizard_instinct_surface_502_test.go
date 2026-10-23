package api

// GitHub #502 (P1, security). The setup routes refused only a role-user
// session, so every API key, a project-scoped one included, could rewrite
// config.yaml (chat endpoint and key, embedding endpoint, dispatcher
// project) and make the daemon dial an endpoint of its choosing. The project
// wizard needed only an operator id, which every key yields, so a tenant
// could commit a project outside its scope. Any caller could retire a
// global instinct. Found at: review (the #495 lane's census). A holder of
// such a key reaches each surface on its HTTP route, so these tests run the
// real router (newHarness495: AuthMiddleware, the configured static keys, a
// DB project key, real SQLite). Design: 18-security-and-tenancy.md,
// "Amendment 2026-10-22 (#502)".

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// setupSurface502 is design U1 rows 1-8: every working setup pair.
var setupSurface502 = []route495{
	{http.MethodGet, "/api/v1/setup/status", ""},
	{http.MethodPost, "/api/v1/setup/models", `{"endpoint":"http://192.0.2.1:11434","api_key":"k"}`},
	{http.MethodPost, "/api/v1/setup/dispatcher", `{"project_id":"alpha"}`},
	{http.MethodPost, "/api/v1/setup/session", `{}`},
	{http.MethodPost, "/api/v1/setup/session/s502/validate", `{"endpoint":"http://192.0.2.1:11434","api_key":"k","model":"m"}`},
	{http.MethodPost, "/api/v1/setup/session/s502/commit", `{"endpoint":"http://192.0.2.1:11434","api_key":"k","model":"m"}`},
	{http.MethodPost, "/api/v1/setup/session/s502/memory/validate", `{"enabled":true}`},
	{http.MethodPost, "/api/v1/setup/session/s502/memory/commit", `{"enabled":true}`},
}

// wizardSurface502 is design S4: every wizard pair, not only commit.
var wizardSurface502 = []route495{
	{http.MethodPost, "/api/v1/projects/wizard/converse", `{"message":"make a project"}`},
	{http.MethodPost, "/api/v1/projects/wizard/converse/", `{"message":"make a project"}`},
	{http.MethodPost, "/api/v1/projects/wizard/s502/commit", `{}`},
	{http.MethodPost, "/api/v1/projects/wizard/s502/cancel", `{}`},
	{http.MethodPost, "/api/v1/projects/wizard/s502/confirm-schedule", `{}`},
}

// assertOperatorSurface502 checks the three answers of the operator gate.
// scopedCode is what a project-scoped key gets: 404 from the gate, or, on
// the wizard paths, 403 from ProjectAuthMiddleware, which reads
// /api/v1/projects/wizard/... as project "wizard" and refuses it first.
func assertOperatorSurface502(t *testing.T, h *harness466, routes []route495, scopedCode int) {
	t.Helper()
	for _, rt := range routes {
		code, body := h.do(t, rt.method, rt.path, staticKey495, rt.body)
		assert.Equal(t, http.StatusForbidden, code, "%s %s served an unscoped non-admin static key: %s", rt.method, rt.path, body)
		assert.Contains(t, body, "OPERATOR_CAPABILITY_REQUIRED", "%s %s", rt.method, rt.path)

		code, body = h.do(t, rt.method, rt.path, h.projectKey, rt.body)
		assert.Equal(t, scopedCode, code, "%s %s served a project-scoped key: %s", rt.method, rt.path, body)

		code, body = h.do(t, rt.method, rt.path, adminKey466, rt.body)
		assert.NotEqual(t, http.StatusUnauthorized, code, "%s %s: %s", rt.method, rt.path, body)
		assert.NotEqual(t, http.StatusForbidden, code, "%s %s: %s", rt.method, rt.path, body)
		assert.NotContains(t, body, "404 page not found", "%s %s: the gate hid the surface from the admin key", rt.method, rt.path)
	}
}

func TestSetupSurface502_OperatorOnly(t *testing.T) {
	assertOperatorSurface502(t, newHarness495(t), setupSurface502, http.StatusNotFound)
}

func TestWizardSurface502_OperatorOnly(t *testing.T) {
	assertOperatorSurface502(t, newHarness495(t), wizardSurface502, http.StatusForbidden)
}

func TestInstinctRetire502_GlobalRowIsOperatorSurface(t *testing.T) {
	ctx := context.Background()
	repo := sqlite.NewInstinctRepository(sqlitetest.Memory(t).DB)
	h := newHarness466With(t, []string{staticKey495}, WithInstinctRepository(repo))
	now := time.Now().UTC()
	seed := func(scope, project, key string) string {
		id, err := repo.Upsert(ctx, &persistence.Instinct{
			Scope: scope, ProjectID: project, Domain: "recovery", TriggerKey: key,
			Action: "retry", Confidence: 0.6, Source: "operator", Status: persistence.InstinctStatusActive,
			CreatedAt: now, UpdatedAt: now, LastSeenAt: now,
		})
		require.NoError(t, err)
		return id
	}
	global := seed(persistence.InstinctScopeGlobal, "", "g502")
	own := seed(persistence.InstinctScopeProject, "alpha", "p502")
	retire := func(id, key string) (int, string) {
		return h.do(t, http.MethodPost, "/api/v1/instincts/"+id+"/retire", key, `{}`)
	}
	status := func(id string) string {
		row, err := repo.Get(ctx, id)
		require.NoError(t, err)
		return row.Status
	}

	code, body := retire(global, staticKey495)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "OPERATOR_CAPABILITY_REQUIRED")
	code, body = retire(global, h.projectKey)
	assert.Equal(t, http.StatusNotFound, code, body)
	assert.Equal(t, persistence.InstinctStatusActive, status(global), "a refused caller retired the global instinct")

	code, body = retire(global, adminKey466)
	assert.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, persistence.InstinctStatusRetired, status(global))

	// A project-scoped row in the key's own project stays the tenant's.
	code, body = retire(own, h.projectKey)
	assert.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, persistence.InstinctStatusRetired, status(own))
}

// S5's tripwire (review 394c F4): ListInstincts gives every authenticated
// caller the global rows, so ShowInstinct of one stays readable. Gating Show
// alone would close nothing (whether tenants read global rows at all is
// #504).
func TestInstinctShow502_GlobalRowStaysReadable(t *testing.T) {
	ctx := context.Background()
	repo := sqlite.NewInstinctRepository(sqlitetest.Memory(t).DB)
	h := newHarness466With(t, []string{staticKey495}, WithInstinctRepository(repo))
	now := time.Now().UTC()
	id, err := repo.Upsert(ctx, &persistence.Instinct{
		Scope: persistence.InstinctScopeGlobal, Domain: "recovery", TriggerKey: "g502show",
		Action: "retry", Confidence: 0.6, Source: "operator", Status: persistence.InstinctStatusActive,
		CreatedAt: now, UpdatedAt: now, LastSeenAt: now,
	})
	require.NoError(t, err)
	for _, key := range []string{h.projectKey, staticKey495} {
		code, body := h.do(t, http.MethodGet, "/api/v1/instincts/"+id, key, "")
		assert.Equal(t, http.StatusOK, code, body)
	}
}

// S6: neither confined key kind reaches a setup or wizard path, so the
// operator gate is the only decision on them.
func TestSetupSurface502_NotInConfinedRouteSets(t *testing.T) {
	for _, rt := range append(append([]route495(nil), setupSurface502...), wizardSurface502...) {
		assert.False(t, IsAgentRoute(rt.path), "%s is an agent route", rt.path)
		assert.False(t, isCompanionAllowedPath(rt.path), "%s is a companion route", rt.path)
	}
}

// Every setup and wizard registration goes through operatorOnly (S1, S4):
// a route registered bare would skip the gate, since the handlers no longer
// carry their own check.
func TestOperatorSurface502_SetupAndWizardRoutesWrapped(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	require.NoError(t, err)
	file, err := parser.ParseFile(token.NewFileSet(), "routes.go", src, 0)
	require.NoError(t, err)
	seen := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		path := strings.Trim(lit.Value, `"`)
		if !strings.HasPrefix(path, "/api/v1/setup") && !strings.HasPrefix(path, "/api/v1/projects/wizard") {
			return true
		}
		seen++
		h := string(src[call.Args[1].Pos()-1 : call.Args[1].End()-1])
		assert.True(t, strings.HasPrefix(h, "server.operatorOnly("), "%s is registered as %s, not through server.operatorOnly", path, h)
		return true
	})
	assert.Equal(t, 7, seen, "the setup and wizard registrations moved: update design U1/U3")
}

// The eight setup handlers lost their in-handler RoleUser check (S1); a
// role-user browser session is refused by the gate instead (review 394c F7,
// 71b2 N6): 403 when it carries no project scope, 404 when it does. This
// replaces the seven *_BlocksProjectScopedUser tests, which ran with auth
// off, where the gate admits every caller by design (IsAdminClassRequest).
func TestSetupSurface502_UserSessionRefusedByTheGate(t *testing.T) {
	srv := NewServer()
	handlers := map[string]http.HandlerFunc{
		"SetupStatus": srv.SetupStatus, "SetupModels": srv.SetupModels,
		"SetupDispatcherCommit": srv.SetupDispatcherCommit, "SetupSessionCreate": srv.SetupSessionCreate,
		"SetupSessionValidate": srv.SetupSessionValidate, "SetupSessionCommit": srv.SetupSessionCommit,
		"SetupMemoryValidate": srv.SetupMemoryValidate, "SetupMemoryCommit": srv.SetupMemoryCommit,
	}
	for name, h := range handlers {
		for _, tc := range []struct {
			scope []string
			want  int
		}{{nil, http.StatusForbidden}, {[]string{"alpha"}, http.StatusNotFound}} {
			ctx := context.WithValue(stampSetupIdentity(context.Background(), "user"), authEnabledKey, true)
			if tc.scope != nil {
				ctx = context.WithValue(ctx, projectIDKey, tc.scope)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/x", strings.NewReader(`{}`)).WithContext(ctx)
			rec := httptest.NewRecorder()
			srv.operatorOnly(h)(rec, req)
			assert.Equal(t, tc.want, rec.Code, "%s, user session with scope %v: %s", name, tc.scope, rec.Body.String())
		}
	}
}

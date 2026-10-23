package api

// GitHub #495 (P1, security): requireOperatorScope admitted every caller that
// carried no project scope, so an api.api_keys static key NOT in
// admin.allowed_keys read the daemon config, governed the memory firewall,
// edited operator profiles and links, read the operator audit, proposed,
// approved, applied and rolled back control-plane changes and widened a
// skill to every project; POST /api/v1/config/reload had no gate at all.
// Found at: review (the #491 lane's adversarial code review). An attacker
// holding such a key reaches it on the HTTP route, so these tests run the
// real router (NewRouter: AuthMiddleware with the configured static keys, a
// DB project key, the project gate) over real SQLite. Design:
// 18-security-and-tenancy.md, "Amendment 2026-10-21 (#495)".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticKey495 is an api.api_keys entry that is NOT in admin.allowed_keys:
// unscoped (BuildAuthConfig maps every configured static key to all
// projects) and not admin-class.
const staticKey495 = "sk-static-not-admin-495"

type route495 struct{ method, path, body string }

// operatorSurface495 is the census of design A3 and A4a: every working
// method/path pair behind the operator-surface gate (29).
var operatorSurface495 = []route495{
	{http.MethodGet, "/api/v1/config", ""},                                                // 1
	{http.MethodGet, "/api/v1/memory/policy/evaluations", ""},                             // 2
	{http.MethodGet, "/api/v1/memory/policy/evaluations.csv", ""},                         // 3
	{http.MethodGet, "/api/v1/memory/policy/evaluations/digest/d495", ""},                 // 4
	{http.MethodGet, "/api/v1/memory/policy/mode", ""},                                    // 5
	{http.MethodPost, "/api/v1/memory/policy/chunks/c495", `{"policy":"allow"}`},          // 6
	{http.MethodPost, "/api/v1/skills/s495/global", `{"global":true}`},                    // 7
	{http.MethodGet, "/api/v1/operator/proposals", ""},                                    // 8
	{http.MethodPost, "/api/v1/operator/proposals", `{"title":"t"}`},                      // 9
	{http.MethodGet, "/api/v1/operator/proposals/p495", ""},                               // 10
	{http.MethodPost, "/api/v1/operator/proposals/p495/decide", `{"decision":"approve"}`}, // 11
	{http.MethodPost, "/api/v1/operator/proposals/p495/apply", `{}`},                      // 12
	{http.MethodPost, "/api/v1/operator/proposals/p495/rollback", `{}`},                   // 13
	{http.MethodPost, "/api/v1/operator/diagnose", `{"focus":"alpha"}`},                   // 14
	{http.MethodGet, "/api/v1/operators", ""},                                             // 15
	{http.MethodGet, "/api/v1/operators/telegram:495", ""},                                // 16
	{http.MethodPost, "/api/v1/operators/telegram:495", `{"key":"k","value":"v"}`},        // 17
	{http.MethodDelete, "/api/v1/operators/telegram:495", `{}`},                           // 18
	{http.MethodGet, "/api/v1/operators/telegram:495/links", ""},                          // 19
	{http.MethodPost, "/api/v1/operators/telegram:495/links", `{}`},                       // 20
	{http.MethodDelete, "/api/v1/operators/telegram:495/links/telegram:9", ""},            // 21
	{http.MethodGet, "/api/v1/operators/telegram:495/audit", ""},                          // 22
	{http.MethodGet, "/api/v1/operator/accounts", ""},                                     // 23
	{http.MethodPost, "/api/v1/operator/accounts", `{}`},                                  // 24
	{http.MethodGet, "/api/v1/operator/accounts/u495", ""},                                // 25
	{http.MethodPost, "/api/v1/operator/accounts/u495/disable", `{}`},                     // 26
	{http.MethodPost, "/api/v1/operator/assist", `{}`},                                    // 27
	{http.MethodPost, "/api/v1/config/reload", ""},                                        // 28
	{http.MethodGet, "/api/v1/config/reload-status", ""},                                  // 29
}

// newHarness495 is the #466 harness (real router, real SQLite, an admin
// static key, a DB project key) with a non-admin static key added and the
// config reload handlers registered (no reloader: they answer 503 past the
// gate).
func newHarness495(t *testing.T) *harness466 {
	t.Helper()
	prev := configHandlers
	SetConfigHandlers(NewConfigHandlers(nil))
	t.Cleanup(func() { SetConfigHandlers(prev) })
	return newHarness466With(t, []string{staticKey495})
}

func TestOperatorSurface495_CensusSize(t *testing.T) {
	require.Len(t, operatorSurface495, 29, "design A3 + A4a: 27 operator pairs plus config reload and reload-status")
}

func TestOperatorSurface495_UnscopedStaticKeyRefused(t *testing.T) {
	h := newHarness495(t)
	for _, rt := range operatorSurface495 {
		code, body := h.do(t, rt.method, rt.path, staticKey495, rt.body)
		assert.Equal(t, http.StatusForbidden, code, "%s %s served an unscoped non-admin static key: %s", rt.method, rt.path, body)
		assert.Contains(t, body, "OPERATOR_CAPABILITY_REQUIRED", "%s %s", rt.method, rt.path)
	}
}

func TestOperatorSurface495_AdminKeyAllowed(t *testing.T) {
	h := newHarness495(t)
	for _, rt := range operatorSurface495 {
		code, body := h.do(t, rt.method, rt.path, adminKey466, rt.body)
		assert.NotEqual(t, http.StatusUnauthorized, code, "%s %s: %s", rt.method, rt.path, body)
		assert.NotEqual(t, http.StatusForbidden, code, "%s %s: %s", rt.method, rt.path, body)
		assert.NotContains(t, body, "OPERATOR_CAPABILITY_REQUIRED", "%s %s", rt.method, rt.path)
		assert.NotContains(t, body, "404 page not found", "%s %s: the gate hid the surface from the admin key", rt.method, rt.path)
	}
}

func TestOperatorSurface495_ScopedKeyHidden(t *testing.T) {
	h := newHarness495(t)
	for _, rt := range operatorSurface495 {
		code, body := h.do(t, rt.method, rt.path, h.projectKey, rt.body)
		assert.Equal(t, http.StatusNotFound, code, "%s %s served a project-scoped key: %s", rt.method, rt.path, body)
	}
}

// Every /api/v1/config* registration is GetConfig (gated in the handler) or
// goes through operatorOnly: config reload shipped with no gate at all
// (design A4a), so a config route registered bare fails here (review F6).
func TestOperatorSurface495_ConfigRoutesWrapped(t *testing.T) {
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
		if !ok || !strings.HasPrefix(strings.Trim(lit.Value, `"`), "/api/v1/config") {
			return true
		}
		seen++
		path := strings.Trim(lit.Value, `"`)
		h := string(src[call.Args[1].Pos()-1 : call.Args[1].End()-1])
		if path == "/api/v1/config" && h == "server.GetConfig" {
			return true
		}
		assert.True(t, strings.HasPrefix(h, "server.operatorOnly("), "%s is registered as %s, not through server.operatorOnly", path, h)
		return true
	})
	assert.Equal(t, 3, seen, "the /api/v1/config* registrations moved: update design A4a")
}

// expectedOperatorGated495 is every function that calls requireOperatorScope
// (design A3, plus the operatorOnly wrapper of O5). A new gated handler
// without a census row fails here.
var expectedOperatorGated495 = []string{
	"AdminMemoryFirewallChunkPolicy",
	"AdminMemoryFirewallEvaluations",
	"AdminMemoryFirewallEvaluationsByDigest",
	"AdminMemoryFirewallEvaluationsCSV",
	"AdminMemoryFirewallMode",
	"CreateOperatorLink",
	"DeleteOperatorLink",
	"ForgetOperator",
	"GetConfig",
	"ListOperatorAudit",
	"ListOperatorLinks",
	"ListOperators",
	"OperatorAccountItem",
	"OperatorAccounts",
	"OperatorAssist",
	"OperatorDiagnose",
	"OperatorProposalItem",
	"OperatorProposals",
	"SetOperatorKey",
	"ShowOperator",
	"SkillSetGlobal",
	"RetireInstinct",
	"operatorOnly",
}

// Limit (code review review-20261022-5c5f, F2): this is a list of names.
// It catches a gated handler with no census row and a stale row. It cannot
// see an operator route that was never gated, because such a route makes no
// requireOperatorScope call. The guard against that is
// TestOperatorSurface495_UnscopedStaticKeyRefused over the census, and the
// census is only as complete as design A3/A4.
func TestOperatorSurface495_CensusCoversEveryGate(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var got []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "requireOperatorScope":
						got = append(got, fn.Name.Name)
					case "requireOperatorCapability":
						t.Errorf("%s calls requireOperatorCapability: the second gate implementation is gone (design O3)", fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(got)
	want := append([]string(nil), expectedOperatorGated495...)
	sort.Strings(want)
	assert.Equal(t, want, got, "the set of handlers behind requireOperatorScope moved: update the design census (A3) and operatorSurface495")
}

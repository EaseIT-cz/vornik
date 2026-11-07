package api

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #560 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1b)"): managed hosting's instance owner (tenant_admin) is granted a
// closed set of named operator surfaces and refused every other one with 403,
// never 404, and is never admin-class. Found at: design (SaaS MVP design 2.2,
// invariant 3).

func sessionRequest(role string, projects []string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(r.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, identityKey, &auth.Identity{
		Subject:  "user:owner",
		Projects: projects,
		Extra:    map[string]any{auth.ExtraSessionRole: role, auth.ExtraSessionID: "s1", auth.ExtraSessionUserID: "owner"},
	})
	if len(projects) > 0 {
		ctx = context.WithValue(ctx, projectIDKey, projects)
	}
	return r.WithContext(ctx)
}

var allSurfaces = []Surface{
	SurfaceWizardConverse, SurfaceWizard, SurfaceMCPOAuthConnect, SurfaceOperatorAudit,
	SurfaceConfigReload, SurfaceConfigReloadStatus, SurfaceSetupStatus, SurfaceSetupModels,
	SurfaceSetupDispatcher, SurfaceSetupSession, SurfaceSetupSessionRouter, SurfaceSetupManaged, SurfaceConfigAssist,
	SurfaceConfigShow, SurfaceInstinctRetireGlobal, SurfaceMemoryFirewall, SurfaceOperatorAccounts,
	SurfaceOperatorLinks, SurfaceOperatorProposals, SurfaceOperators, SurfaceSkillSetGlobal,
	SurfaceOperatorCredentials, SurfaceConfigJournal, SurfaceInflight, SurfaceAgentGrant,
}

var grantedSurfaces = map[Surface]bool{
	SurfaceWizardConverse: true, SurfaceWizard: true, SurfaceMCPOAuthConnect: true, SurfaceOperatorAudit: true,
	SurfaceOperatorCredentials: true, // GitHub #623
}

// B3: exactly five surfaces are granted (the fifth, operator_credentials,
// GitHub #623).
func TestTenantAdmin560_Allowlist(t *testing.T) {
	n := 0
	for _, s := range allSurfaces {
		if TenantAdminMay(s) != grantedSurfaces[s] {
			t.Errorf("TenantAdminMay(%s) = %v, want %v", s, TenantAdminMay(s), grantedSurfaces[s])
		}
		if TenantAdminMay(s) {
			n++
		}
	}
	if n != 5 || len(tenantAdminSurfaces) != 5 {
		t.Fatalf("granted %d surfaces (map holds %d), want 5", n, len(tenantAdminSurfaces))
	}
	if TenantAdminMay(Surface("unlisted")) {
		t.Fatal("an unlisted surface was granted")
	}
}

// B2: admin-class first, then tenant_admin, then the project scope. An owner's
// ["*"] session reads as scoped, so a tenant_admin checked after the scope
// branch would be hidden (404); it must get its answer (allowed or 403).
func TestTenantAdmin560_DecisionOrder(t *testing.T) {
	for _, projects := range [][]string{{"*"}, {"p1"}} {
		r := sessionRequest(auth.RoleTenantAdmin, projects)
		if _, scoped := requestScopedProjectSet(r); !scoped {
			t.Fatalf("precondition: a %v session should read as scoped", projects)
		}
		if IsAdminClassRequest(r, nil) {
			t.Fatal("tenant_admin is admin-class")
		}
		for _, s := range allSurfaces {
			got := OperatorSurfaceAccessFor(r, nil, s)
			want := OperatorSurfaceRefused
			if grantedSurfaces[s] {
				want = OperatorSurfaceAllowed
			}
			if got != want {
				t.Errorf("%v tenant_admin on %s: %v, want %v", projects, s, got, want)
			}
		}
	}
	// Unchanged for everyone else: an admin session passes every surface, a
	// scoped user is hidden, an unscoped user refused.
	for _, s := range allSurfaces {
		if OperatorSurfaceAccessFor(sessionRequest(auth.RoleAdmin, []string{"*"}), nil, s) != OperatorSurfaceAllowed {
			t.Errorf("admin session refused %s", s)
		}
		if OperatorSurfaceAccessFor(sessionRequest(auth.RoleUser, []string{"p1"}), nil, s) != OperatorSurfaceHidden {
			t.Errorf("scoped user not hidden on %s", s)
		}
		if OperatorSurfaceAccessFor(sessionRequest(auth.RoleUser, nil), nil, s) != OperatorSurfaceRefused {
			t.Errorf("unscoped user not refused on %s", s)
		}
	}
}

// B2c: the gate answers 403 to a refused tenant_admin, passes a granted one,
// and logs each tenant_admin decision with its surface.
func TestTenantAdmin560_GateLogsDecisions(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{logger: zerolog.New(&logs)}
	w := httptest.NewRecorder()
	if s.requireOperatorScopeFor(w, sessionRequest(auth.RoleTenantAdmin, []string{"*"}), SurfaceConfigShow) || w.Code != http.StatusForbidden {
		t.Fatalf("refused surface: passed or status %d", w.Code)
	}
	if !s.requireOperatorScopeFor(httptest.NewRecorder(), sessionRequest(auth.RoleTenantAdmin, []string{"*"}), SurfaceWizard) {
		t.Fatal("granted surface refused")
	}
	out := logs.String()
	for _, want := range []string{`"surface":"config_show"`, `"outcome":"refused"`, `"surface":"wizard"`, `"outcome":"allowed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("decision log lacks %s: %s", want, out)
		}
	}
}

// The MCP OAuth connect gate grants tenant_admin by surface; the admin-only
// families still refuse it.
func TestTenantAdmin560_MCPOAuthAndAdminOnlySurfaces(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	if !s.requireOperatorScopeFor(httptest.NewRecorder(), sessionRequest(auth.RoleTenantAdmin, []string{"*"}), SurfaceMCPOAuthConnect) {
		t.Fatal("tenant_admin refused MCP OAuth connect")
	}
	// GitHub #503: the admin-only routes that had their own gate now pass
	// the one decision with surfaces tenant_admin is not granted.
	for _, surface := range []Surface{SurfaceConfigJournal, SurfaceInflight, SurfaceAgentGrant} {
		w := httptest.NewRecorder()
		if s.requireOperatorScopeFor(w, sessionRequest(auth.RoleTenantAdmin, []string{"*"}), surface) || w.Code != http.StatusForbidden {
			t.Fatalf("%s admitted tenant_admin (status %d)", surface, w.Code)
		}
	}
}

// B5 completeness: the surfaces passed to the three surface-taking gates
// (operatorOnly's first argument, requireOperatorScopeFor's and
// OperatorSurfaceAccessFor's third) are exactly the declared Surface
// constants, each used at least once.
func TestTenantAdmin560_SurfaceCompleteness(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// A surface may also be gated from the console package, through
	// api.OperatorSurfaceAccessFor (GitHub #623, /ui/credentials).
	uiFiles, err := filepath.Glob("../ui/*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, uiFiles...)
	fset := token.NewFileSet()
	declared := map[string]bool{}
	used := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				if id, ok := x.Type.(*ast.Ident); ok && id.Name == "Surface" {
					for _, nm := range x.Names {
						declared[nm.Name] = true
					}
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				idx := -1
				switch sel.Sel.Name {
				case "operatorOnly":
					idx = 0
				case "requireOperatorScopeFor", "OperatorSurfaceAccessFor":
					idx = 2
				}
				if idx < 0 || len(x.Args) <= idx {
					return true
				}
				arg := x.Args[idx]
				if sel, isSel := arg.(*ast.SelectorExpr); isSel {
					arg = sel.Sel // api.SurfaceX from the console package
				}
				id, ok := arg.(*ast.Ident)
				if !ok {
					t.Errorf("%s: %s is passed a non-constant surface", fset.Position(x.Pos()), sel.Sel.Name)
					return true
				}
				used[id.Name]++
			}
			return true
		})
	}
	// The gate definitions pass their surface parameter through by name.
	delete(used, "surface")
	if len(declared) != len(allSurfaces) {
		t.Errorf("%d Surface constants declared, the test lists %d", len(declared), len(allSurfaces))
	}
	for name := range used {
		if !declared[name] {
			t.Errorf("a gate is passed %s, which is not a declared Surface constant", name)
		}
	}
	for name := range declared {
		if used[name] == 0 {
			t.Errorf("Surface %s is declared but no gate uses it", name)
		}
	}
	t.Logf("surface completeness: %d constants, %d distinct used", len(declared), len(used))
}

// tenantSessionBackend authenticates one cookie as an admin principal's
// session demoted to tenant_admin, as M1a's backend does off the operations
// listener.
type tenantSessionBackend struct{ role string }

func (tenantSessionBackend) Name() string { return "session" }
func (b tenantSessionBackend) Authenticate(_ context.Context, cred auth.Credential) (*auth.Identity, error) {
	if cred.SessionToken != "owner-cookie" {
		return nil, auth.ErrNoCredential
	}
	return &auth.Identity{
		Subject:  "user:owner",
		Projects: []string{"*"},
		Extra:    map[string]any{auth.ExtraSessionRole: b.role, auth.ExtraSessionID: "s1", auth.ExtraSessionUserID: "owner"},
	}, nil
}

// B5 matrix, through the real router: a tenant_admin session on every
// operator-surface route gets exactly B3's answer (403 on a refused surface,
// past the gate on a granted one, never the gate's hidden 404), and each
// decision is logged.
func TestTenantAdmin560_Matrix(t *testing.T) {
	var logs bytes.Buffer
	prev := configHandlers
	SetConfigHandlers(NewConfigHandlers(nil)) // before the router is built, as newHarness495 does
	t.Cleanup(func() { SetConfigHandlers(prev) })
	h := newHarness466With(t, nil, WithServerSessionBackend(tenantSessionBackend{role: auth.RoleTenantAdmin}), WithLogger(zerolog.New(&logs)))

	granted := func(rt route495) bool {
		return strings.HasPrefix(rt.path, "/api/v1/projects/wizard/") || strings.HasSuffix(rt.path, "/audit") ||
			strings.HasPrefix(rt.path, "/api/v1/mcp/oauth/")
	}
	routes := append(append(append([]route495{}, operatorSurface495...), setupSurface502...), wizardSurface502...)
	// The MCP OAuth connect routes (granted) are in operatorSurface495 since
	// GitHub #503, which moved them onto the one operator-surface gate.
	nGranted := 0
	for _, rt := range routes {
		req, err := http.NewRequest(rt.method, h.hs.URL+rt.path, strings.NewReader(rt.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "owner-cookie"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		_, _ = body.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		gateRefused := resp.StatusCode == http.StatusForbidden &&
			(strings.Contains(body.String(), "OPERATOR_CAPABILITY_REQUIRED") || strings.Contains(body.String(), "ADMIN_SCOPE_REQUIRED"))
		gateHidden := resp.StatusCode == http.StatusNotFound && strings.HasPrefix(body.String(), "404 page not found")
		if gateHidden {
			t.Errorf("%s %s: hidden (404) from tenant_admin; the owner gets an answer, not a hidden surface", rt.method, rt.path)
			continue
		}
		if granted(rt) {
			nGranted++
			if gateRefused {
				t.Errorf("%s %s: refused a granted surface: %d %s", rt.method, rt.path, resp.StatusCode, body.String())
			}
		} else if !gateRefused {
			t.Errorf("%s %s: tenant_admin reached a refused surface: %d %s", rt.method, rt.path, resp.StatusCode, body.String())
		}
	}
	if nGranted != 9 {
		t.Errorf("%d granted routes exercised, want 9 (5 wizard, 1 audit, 3 MCP OAuth)", nGranted)
	}
	// An admin-only (requireAdminGate) route refuses the owner.
	req, _ := http.NewRequest(http.MethodGet, h.hs.URL+"/api/v1/admin/audit", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "owner-cookie"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Errorf("tenant_admin reached the admin-only /api/v1/admin/audit: %d", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "operator surface: tenant_admin refused") || !strings.Contains(logs.String(), "operator surface: tenant_admin allowed") {
		t.Error("tenant_admin decisions were not logged")
	}
	t.Logf("matrix: %d routes, %d granted", len(routes), nGranted)
}

// B4: the credential exchange's nav marker follows the session backend's
// demotion.
func TestTenantAdmin560_NavRole(t *testing.T) {
	sb := authsession.NewSessionBackend(nil, nil, 0)
	s := &Server{sessionBackend: sb}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := s.navRole(r, auth.RoleAdmin); got != auth.RoleAdmin {
		t.Fatalf("self-hosted admin nav role %q", got)
	}
	sb.DemoteAdmin = func(context.Context) bool { return true }
	if got := s.navRole(r, auth.RoleAdmin); got != auth.RoleUser {
		t.Fatalf("demoted admin nav role %q, want user", got)
	}
	if got := s.navRole(r, auth.RoleUser); got != auth.RoleUser {
		t.Fatalf("user nav role %q", got)
	}
}

// B2b: the owner sees every reminder on the instance, operator-owned ones
// included (review f715 F6, 2fd9 F2).
func TestTenantAdmin560_RemindersAllRows(t *testing.T) {
	s := &Server{}
	rem := &persistence.Reminder{ID: "r1", OperatorID: "telegram:42", Channel: "telegram"}
	if !s.reminderVisibleToRequest(sessionRequest(auth.RoleTenantAdmin, []string{"*"}), rem) {
		t.Fatal("tenant_admin cannot see an operator-owned reminder")
	}
	if s.reminderVisibleToRequest(sessionRequest(auth.RoleUser, []string{"p1"}), rem) {
		t.Fatal("a scoped user sees an operator-owned reminder")
	}
}

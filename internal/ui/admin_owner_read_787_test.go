package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
)

// GitHub #787 (18-security-and-tenancy.md, amendment 2026-11-15 (#787)).

// ownerConsoleActionPrefixes are router prefixes that dispatch only actions
// (never a page the owner reads), each refused by OwnerConsoleRead because
// no readable entry or detail pattern covers it.
var ownerConsoleActionPrefixes = map[string]string{
	"/users/":                         "accounts: refused, with /users",
	"/cpc/":                           "only /cpc/<id>/cancel",
	"/instincts/":                     "only /instincts/<id>/retire",
	"/blackbox/triggers/bulk-dismiss": "an action",
	"/agents/":                        "covered by the detail pattern; /models/withdraw is an action",
}

// The census (review fdf7 F2, 8f97 F3): every dispatch literal in
// adminRouter, whatever its matcher (case label, path ==, HasPrefix,
// CutPrefix), is classified. A new page fails here until someone decides
// whether the owner may read it.
func TestOwnerConsoleRead787_EveryDispatchSiteIsClassified(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "admin.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var router *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "adminRouter" {
			router = fd
		}
	}
	if router == nil {
		t.Fatal("adminRouter not found")
	}
	lit := func(e ast.Expr) (string, bool) {
		bl, ok := e.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(bl.Value)
		return v, err == nil
	}
	var sites []string
	ast.Inspect(router.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CaseClause:
			for _, e := range x.List {
				if v, ok := lit(e); ok {
					sites = append(sites, v)
				}
			}
		case *ast.BinaryExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "path" && x.Op == token.EQL {
				if v, ok := lit(x.Y); ok {
					sites = append(sites, v)
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "HasPrefix" || sel.Sel.Name == "CutPrefix") && len(x.Args) == 2 {
				if id, ok := x.Args[0].(*ast.Ident); ok && id.Name == "path" {
					if v, ok := lit(x.Args[1]); ok {
						sites = append(sites, v)
					}
				}
			}
		}
		return true
	})
	// 61 literals at the commit that added this test; a scan that loses a
	// matcher shape (a renamed variable) drops well below (review b8ba F4).
	if len(sites) < 55 {
		t.Fatalf("found only %d dispatch literals; the scan is broken", len(sites))
	}
	for _, site := range sites {
		p := site
		if len(p) > 1 {
			p = strings.TrimSuffix(p, "/")
		}
		_, detail := api.OwnerConsoleDetailPatterns[site]
		_, action := ownerConsoleActionPrefixes[site]
		if site == "" || api.OwnerConsoleReadablePaths[p] || api.OwnerConsoleRefusedPaths[p] || detail || action {
			continue
		}
		t.Errorf("adminRouter dispatches %q, which the owner's console read does not classify (readable, refused, detail pattern or action prefix)", site)
	}
	t.Logf("classified %d dispatch literals", len(sites))
}

// The marker is presentation only, inserted after <body>.
func TestOwnerReadOnlyWriter787_InsertsTheMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &ownerReadOnlyWriter{ResponseWriter: rec}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<html><body class="x"><main><form method="POST"></form></main></body></html>`))
	w.flush()
	out := rec.Body.String()
	if !strings.Contains(out, `<body class="x"><div data-console-readonly`) || !strings.Contains(out, `form[method="post" i]`) {
		t.Fatalf("marker not inserted after <body>: %s", out)
	}
}

// The router owns the role check too (review fdf7 F1): a gate that marked a
// non-owner read-only still meets 403 at the router.
func TestOwnerConsoleRead787_RouterRefusesAMarkedNonOwner(t *testing.T) {
	s := NewServer()
	markAll := admin.Middleware(config.AdminConfig{Enabled: true}, nil, func(*http.Request) bool { return true },
		func(*http.Request) bool { return false }, "/admin",
		admin.WithOwnerRead(func(*http.Request) bool { return true }))(http.HandlerFunc(s.adminRouter))
	for _, role := range []string{auth.RoleUser, auth.RoleTenantAdmin} {
		req := setupAuthRequest(http.MethodGet, "/admin/audit", role)
		rec := httptest.NewRecorder()
		markAll.ServeHTTP(rec, req)
		want := http.StatusForbidden
		if role == auth.RoleTenantAdmin {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Errorf("%s marked read-only, GET /admin/audit: %d, want %d", role, rec.Code, want)
		}
	}
	// A marked owner's POST is refused by the router as well.
	req := setupAuthRequest(http.MethodPost, "/admin/audit", auth.RoleTenantAdmin)
	rec := httptest.NewRecorder()
	markAll.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("marked owner POST: %d, want 403", rec.Code)
	}
}

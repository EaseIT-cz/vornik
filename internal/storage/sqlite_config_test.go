package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"vornik.io/vornik/internal/config"
)

// Both SQLite call sites (openSQLite and OpenReadOnly) build their
// sqlite.Config through sqliteConfig, and it must leave ConnectTimeout zero:
// a non-zero value would be case 1 of the connect-deadline rule and re-clamp
// every deadline-bearing CLI caller to it (storage-abstraction design,
// 2026-09-24, review round 1 F1).
func TestSQLiteConfig_LeavesTheConnectBoundToTheCaller(t *testing.T) {
	c := sqliteConfig(config.DatabaseConfig{Driver: "sqlite", Path: "/x/v.db"})
	if c.Path != "/x/v.db" {
		t.Errorf("path = %q", c.Path)
	}
	if c.ConnectTimeout != 0 {
		t.Errorf("ConnectTimeout = %v, want 0 — the caller's deadline must bound the open", c.ConnectTimeout)
	}
}

// The helper only guards the seam if the call sites use it: storage.go builds
// no sqlite.Config literal outside sqliteConfig (round 3 F3), so a site cannot
// inline one with a ConnectTimeout that re-clamps the caller.
func TestSQLiteConfig_IsTheOnlyConstructionSite(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "storage.go", nil, 0)
	if err != nil {
		t.Fatalf("parse storage.go: %v", err)
	}
	var sawHelper bool
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == "sqliteConfig" {
			sawHelper = true
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if cl, ok := n.(*ast.CompositeLit); ok {
				if sel, ok := cl.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Config" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "sqlite" {
						t.Errorf("%s: %s builds a sqlite.Config literal; use sqliteConfig(cfg)", fset.Position(cl.Pos()), fn.Name.Name)
					}
				}
			}
			return true
		})
	}
	if !sawHelper {
		t.Fatal("sqliteConfig not found in storage.go; this guard examined nothing")
	}
}

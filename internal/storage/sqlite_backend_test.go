package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// #367 (2026-10-14): internal/service unit-test containers take a copy of the
// migrated in-memory image instead of storage.Open migrating from empty, and
// wrap it with NewSQLiteBackend. That is only the same container if
// NewSQLiteBackend is what openSQLite builds after connect + migrate: the same
// driver, the same repositories, and Close/Migrate/IsReady bound to the
// database (storage-abstraction design, "In-memory test databases").
func TestNewSQLiteBackend_IsWhatOpenBuildsAfterMigrate(t *testing.T) {
	opened, err := Open(sqlitetest.Context(t), config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	db := sqlitetest.Memory(t)
	wrapped := NewSQLiteBackend(db)

	if wrapped.Driver != opened.Driver || wrapped.PG != nil {
		t.Fatalf("driver %q (PG %v), want %q and no Postgres handle", wrapped.Driver, wrapped.PG, opened.Driver)
	}
	if wrapped.DB != db.DB {
		t.Fatal("the backend's DB is not the database it was given")
	}
	if wrapped.Close == nil || wrapped.Migrate == nil || wrapped.IsReady == nil {
		t.Fatal("Close, Migrate and IsReady must all be bound")
	}
	ow, ov := reflect.ValueOf(*wrapped.Repos), reflect.ValueOf(*opened.Repos)
	checked := 0
	for i := 0; i < ov.NumField(); i++ {
		name := ov.Type().Field(i).Name
		if ov.Field(i).IsNil() != ow.Field(i).IsNil() {
			t.Errorf("repository %s: nil after Open=%v, nil from NewSQLiteBackend=%v", name, ov.Field(i).IsNil(), ow.Field(i).IsNil())
		}
		if ow.Field(i).Kind() == reflect.Interface && !ow.Field(i).IsNil() && ow.Field(i).Elem().Type() != ov.Field(i).Elem().Type() {
			t.Errorf("repository %s: %s from NewSQLiteBackend, %s from Open", name, ow.Field(i).Elem().Type(), ov.Field(i).Elem().Type())
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("compared %d repository fields; the walk reached too few to prove anything", checked)
	}
	t.Logf("compared %d repository fields", checked)
	if err := wrapped.IsReady(sqlitetest.Context(t)); err != nil {
		t.Fatalf("IsReady on the wrapped migrated copy: %v", err)
	}
	if err := wrapped.Migrate(sqlitetest.Context(t)); err != nil {
		t.Fatalf("Migrate on the wrapped migrated copy: %v", err)
	}
}

// #418 (2026-10-17): initDatabase skips its own Migrate when the open already
// migrated. Only openSQLite may say so: it migrates and refuses to return a
// database it could not migrate. A database merely connected and wrapped
// (NewSQLiteBackend over an unmigrated Connect) must not claim it, or a boot
// would run on an empty schema (storage-abstraction design, "The second SQLite
// migrate at boot", 2026-10-17).
func TestMigratedAtOpen_OnlyOpenSQLiteSetsIt(t *testing.T) {
	opened, err := Open(sqlitetest.Context(t), config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	if !opened.MigratedAtOpen {
		t.Fatal("storage.Open's SQLite backend migrated but does not say so")
	}
	if err := opened.IsReady(sqlitetest.Context(t)); err != nil {
		t.Fatalf("a backend claiming MigratedAtOpen is not at the latest schema: %v", err)
	}
	if NewSQLiteBackend(sqlitetest.Connect(t, ":memory:")).MigratedAtOpen {
		t.Fatal("NewSQLiteBackend over an unmigrated database claims it was migrated")
	}

	// The law: no other non-test code in this package sets the flag.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var sites []string
	examined := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		examined++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok && k.Name == "MigratedAtOpen" {
						sites = append(sites, fn.Name.Name)
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "MigratedAtOpen" {
							sites = append(sites, fn.Name.Name)
						}
					}
				}
				return true
			})
		}
	}
	t.Logf("MigratedAtOpen law: %d non-test files examined, set in %v", examined, sites)
	if examined < 3 || strings.Join(sites, ",") != "openSQLite" {
		t.Fatalf("MigratedAtOpen is set in %v (of %d files); want only openSQLite", sites, examined)
	}
}

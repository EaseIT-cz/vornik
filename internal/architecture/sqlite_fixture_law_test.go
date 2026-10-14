package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The SQLite fixture law (storage-abstraction design, "SQLite test fixtures",
// 2026-10-03). Test code opened SQLite by hand at 56 call sites, each with a
// 5 s connect bound, and three of them timed out under parallel `make test`
// load on one day (brokergrants, approverdevice; 09-23/24 internal/cli before
// them). Every test now opens through internal/persistence/sqlite/sqlitetest
// (or, inside the sqlite package, DefaultConfig/FixtureConfig), which carry the
// 60 s test bound. This law keeps it that way:
//
//   - outside internal/persistence/sqlite, no test calls sqlite.Connect
//     (under any import name);
//   - no test builds a sqlite.Config literal — a literal is how a 5 s or
//     default bound gets back in — except the two files that test Connect's
//     own bounds and defaults.
//
// Module-agnostic (matches the import path by suffix), so it ships in the
// Community export.

const sqlitePkgSuffix = "/internal/persistence/sqlite"

// sqliteFixtureExempt are the test files that exercise Connect itself, where
// a literal Config is the subject under test.
var sqliteFixtureExempt = map[string]string{
	"internal/persistence/sqlite/connect_deadline_test.go": "the connect-bound precedence rules",
	"internal/persistence/sqlite/final_coverage_test.go":   "the empty-config defaults and a bad path",
}

type sqliteFixtureCounts struct{ files, connects, literals, storageOpens int }

// sqliteFixtureViolations judges one test file. rel is module-relative with
// forward slashes. Separated from the walk so planted sources can prove it
// fails.
func sqliteFixtureViolations(rel string, src []byte, n *sqliteFixtureCounts) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	n.files++
	dir := filepath.ToSlash(filepath.Dir(rel))
	inSQLitePkg := dir == "internal/persistence/sqlite"
	inFixturePkg := dir == "internal/persistence/sqlite/sqlitetest"

	// The names under which this file can reach the sqlite package: its import
	// name, and — for the package's own in-package tests — the bare identifier.
	local := ""
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !strings.HasSuffix(p, sqlitePkgSuffix) {
			continue
		}
		local = "sqlite"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	bare := inSQLitePkg && f.Name.Name == "sqlite"
	if local == "" && !bare {
		return nil, nil
	}
	isSQLite := func(e ast.Expr, name string) bool {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			return ok && local != "" && id.Name == local && x.Sel.Name == name
		case *ast.Ident:
			return bare && x.Name == name
		}
		return false
	}

	var out []string
	ast.Inspect(f, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.CallExpr:
			if isSQLite(x.Fun, "Connect") {
				n.connects++
				if !inSQLitePkg && !inFixturePkg {
					out = append(out, rel+": calls sqlite.Connect; open through sqlitetest.Open/Connect/Memory/File")
				}
			}
		case *ast.CompositeLit:
			if x.Type != nil && isSQLite(x.Type, "Config") {
				n.literals++
				if _, ok := sqliteFixtureExempt[rel]; !ok && !inFixturePkg {
					out = append(out, rel+": builds a sqlite.Config literal; use sqlite.FixtureConfig/DefaultConfig or sqlitetest")
				}
			}
		}
		return true
	})
	return out, nil
}

// The third rule (storage-abstraction design, "storage.Open in tests",
// 2026-10-12, #126). storage.Open/OpenReadOnly build their sqlite.Config with
// no ConnectTimeout on purpose, so the caller's context is the bound: on
// context.Background() that is the daemon's 5 s default, and
// internal/storage's own TestSQLiteOpensWithSynchronousFull and
// TestOpenReadOnly_SQLiteDoesNotMigrate timed out at it under parallel load
// (2026-10-04, 2026-10-07). A test's storage open must take a sanctioned
// context: sqlitetest.Context(t) or internal/cli's sqliteOpenCtx(t), or an
// identifier assigned from one (directly or through a context.With* of one).
// An allowlist: an unrecognised context fails, it is not "probably fine".

const (
	storagePkgSuffix    = "/internal/storage"
	sqlitetestPkgSuffix = "/internal/persistence/sqlite/sqlitetest"
)

// storageImportNames resolves the names under which a file reaches the
// storage package, the sqlitetest package and the context package.
func storageImportNames(f *ast.File) (storageLocal, fixtureLocal, ctxLocal string) {
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		}
		pick := func(def string) string {
			if name != "" {
				return name
			}
			return def
		}
		switch {
		case strings.HasSuffix(p, storagePkgSuffix):
			storageLocal = pick("storage")
		case strings.HasSuffix(p, sqlitetestPkgSuffix):
			fixtureLocal = pick("sqlitetest")
		case p == "context":
			ctxLocal = pick("context")
		}
	}
	return storageLocal, fixtureLocal, ctxLocal
}

// sanctionedCtx judges one function body's contexts. Assignments are seen in
// source order, so an identifier is sanctioned by its latest assignment.
type sanctionedCtx struct {
	fixtureLocal, ctxLocal string
	ids                    map[string]bool
}

func (s *sanctionedCtx) expr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return s.ids[x.Name]
	case *ast.ParenExpr:
		return s.expr(x.X)
	case *ast.CallExpr:
		switch fn := x.Fun.(type) {
		case *ast.Ident:
			return fn.Name == "sqliteOpenCtx"
		case *ast.SelectorExpr:
			id, ok := fn.X.(*ast.Ident)
			if !ok {
				return false
			}
			if s.fixtureLocal != "" && id.Name == s.fixtureLocal && fn.Sel.Name == "Context" {
				return true
			}
			if s.ctxLocal != "" && id.Name == s.ctxLocal && strings.HasPrefix(fn.Sel.Name, "With") && len(x.Args) > 0 {
				return s.expr(x.Args[0])
			}
		}
	}
	return false
}

// assign records lhs = rhs. With one multi-value call on the right
// (ctx, cancel := context.WithTimeout(...)) the context is the first value.
func (s *sanctionedCtx) assign(lhs, rhs []ast.Expr) {
	if len(rhs) == 0 {
		return
	}
	if len(lhs) == len(rhs) {
		for i, l := range lhs {
			if id, ok := l.(*ast.Ident); ok {
				s.ids[id.Name] = s.expr(rhs[i])
			}
		}
		return
	}
	if id, ok := lhs[0].(*ast.Ident); ok {
		s.ids[id.Name] = s.expr(rhs[0])
	}
}

func storageOpenViolations(rel string, src []byte, n *sqliteFixtureCounts) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	storageLocal, fixtureLocal, ctxLocal := storageImportNames(f)
	bare := dir == "internal/storage" && f.Name.Name == "storage"
	if storageLocal == "" && !bare {
		return nil, nil
	}
	isStorageOpen := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			return ok && storageLocal != "" && id.Name == storageLocal && (x.Sel.Name == "Open" || x.Sel.Name == "OpenReadOnly")
		case *ast.Ident:
			return bare && (x.Name == "Open" || x.Name == "OpenReadOnly")
		}
		return false
	}
	var out []string
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		s := &sanctionedCtx{fixtureLocal: fixtureLocal, ctxLocal: ctxLocal, ids: map[string]bool{}}
		ast.Inspect(fd.Body, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.AssignStmt:
				s.assign(x.Lhs, x.Rhs)
			case *ast.ValueSpec:
				lhs := make([]ast.Expr, len(x.Names))
				for i, nm := range x.Names {
					lhs[i] = nm
				}
				s.assign(lhs, x.Values)
			case *ast.CallExpr:
				if isStorageOpen(x.Fun) {
					n.storageOpens++
					if len(x.Args) == 0 || !s.expr(x.Args[0]) {
						out = append(out, fmt.Sprintf("%s: a storage open on an unsanctioned context; pass sqlitetest.Context(t) (or a context derived from it) so the fixture bound, not the daemon's 5 s, reaches the open", fset.Position(x.Pos())))
					}
				}
			}
			return true
		})
	}
	return out, nil
}

func TestSQLiteFixtureLaw(t *testing.T) {
	root := moduleRoot(t)
	var n sqliteFixtureCounts
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		v, err := sqliteFixtureViolations(filepath.ToSlash(rel), src, &n)
		if err != nil {
			return err
		}
		violations = append(violations, v...)
		v, err = storageOpenViolations(filepath.ToSlash(rel), src, &n)
		if err != nil {
			return err
		}
		violations = append(violations, v...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The denominator: a law that parsed nothing would report clean.
	t.Logf("sqlite fixture law: %d test files parsed, %d sqlite.Connect calls, %d sqlite.Config literals and %d storage opens examined", n.files, n.connects, n.literals, n.storageOpens)
	if n.files < 500 || n.connects == 0 || n.storageOpens == 0 {
		t.Fatalf("examined too little to judge (%d files, %d Connect calls): the walk is not reaching the module's tests", n.files, n.connects)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// The judgement fails on each planted shape and passes on the sanctioned ones.
func TestSQLiteFixtureLaw_CatchesPlantedViolators(t *testing.T) {
	const imp = `import sqlite "vornik.io/vornik/internal/persistence/sqlite"` + "\n"
	cases := []struct {
		name, rel, src string
		want           int
	}{
		{"external Connect", "internal/foo/a_test.go", "package foo\n" + imp + "func f() { sqlite.Connect(nil, sqlite.DefaultConfig()) }", 1},
		{"aliased Connect and literal", "internal/foo/b_test.go", "package foo\nimport repo \"x/internal/persistence/sqlite\"\nfunc f() { repo.Connect(nil, repo.Config{Path: \"p\"}) }", 2},
		{"in-package literal", "internal/persistence/sqlite/c_test.go", "package sqlite\nfunc f() { Connect(nil, Config{Path: \":memory:\"}) }", 1},
		{"external-test-package literal in the sqlite dir", "internal/persistence/sqlite/d_test.go", "package sqlite_test\n" + imp + "func f() { sqlite.Connect(nil, sqlite.Config{}) }", 1},
		{"exempt file", "internal/persistence/sqlite/final_coverage_test.go", "package sqlite_test\n" + imp + "func f() { sqlite.Connect(nil, sqlite.Config{}) }", 0},
		{"sanctioned in-package", "internal/persistence/sqlite/e_test.go", "package sqlite\nfunc f() { Connect(nil, DefaultConfig()); Connect(nil, FixtureConfig(\"p\")) }", 0},
		{"unrelated Config", "internal/foo/g_test.go", "package foo\nimport \"x/internal/config\"\nfunc f() { _ = config.Config{} }", 0},
	}
	for _, c := range cases {
		var n sqliteFixtureCounts
		got, err := sqliteFixtureViolations(c.rel, []byte(c.src), &n)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: %d violations %v, want %d", c.name, len(got), got, c.want)
		}
	}
}

// The storage-open rule fails on each unsanctioned shape and passes on the
// sanctioned ones (audit 2026-10-12: storage.Open in tests took the daemon's
// 5 s bound, #126).
func TestSQLiteFixtureLaw_StorageOpenRule(t *testing.T) {
	const imps = "import (\n\"context\"\n\"x/internal/storage\"\n\"x/internal/persistence/sqlite/sqlitetest\"\n)\n"
	cases := []struct {
		name, rel, src string
		want           int
	}{
		{"background", "internal/foo/a_test.go", "package foo\n" + imps + "func f() { storage.Open(context.Background(), c) }", 1},
		{"ident from background", "internal/foo/b_test.go", "package foo\n" + imps + "func f() { ctx := context.Background(); storage.OpenReadOnly(ctx, c) }", 1},
		{"in-package bare", "internal/storage/c_test.go", "package storage\nimport \"context\"\nfunc f() { ctx := context.Background(); Open(ctx, c) }", 1},
		{"aliased storage", "internal/foo/d_test.go", "package foo\nimport st \"x/internal/storage\"\nfunc f() { st.Open(nil, c) }", 1},
		{"fixture context", "internal/foo/e_test.go", "package foo\n" + imps + "func f() { storage.Open(sqlitetest.Context(t), c) }", 0},
		{"ident from fixture", "internal/foo/g_test.go", "package foo\n" + imps + "func f() { ctx := sqlitetest.Context(t); storage.Open(ctx, c) }", 0},
		{"derived from fixture", "internal/foo/h_test.go", "package foo\n" + imps + "func f() { ctx, cancel := context.WithTimeout(sqlitetest.Context(t), d); defer cancel(); storage.Open(ctx, c) }", 0},
		{"cli helper", "internal/cli/i_test.go", "package cli\n" + imps + "func f() { ctx := sqliteOpenCtx(t); storage.Open(ctx, c) }", 0},
		{"reassigned to background", "internal/foo/j_test.go", "package foo\n" + imps + "func f() { ctx := sqlitetest.Context(t); ctx = context.Background(); storage.Open(ctx, c) }", 1},
		{"a different package is not storage", "internal/foo/k_test.go", "package foo\nimport \"x/internal/storagex\"\nfunc f() { storagex.Open(nil, c) }", 0},
	}
	for _, c := range cases {
		var n sqliteFixtureCounts
		got, err := storageOpenViolations(c.rel, []byte(c.src), &n)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: %d violations %v, want %d", c.name, len(got), got, c.want)
		}
	}
}

package architecture

import (
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

type sqliteFixtureCounts struct{ files, connects, literals int }

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
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The denominator: a law that parsed nothing would report clean.
	t.Logf("sqlite fixture law: %d test files parsed, %d sqlite.Connect calls and %d sqlite.Config literals examined", n.files, n.connects, n.literals)
	if n.files < 500 || n.connects == 0 {
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

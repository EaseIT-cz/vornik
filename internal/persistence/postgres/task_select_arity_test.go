package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestTaskSelectArityMatchesScanTask guards every query whose rows are read
// by scanTask: the number of columns the SQL selects (or RETURNs) must equal
// the number of destinations scanTask passes to Scan.
//
// #334 audit 2026-10-14: GetDependencies selected 31 columns while scanTask
// read 33 (created_by_actor and goal_origin were missing), so every Postgres
// call failed with a scan-arity error. The sqlmock tests could not see it —
// a mock returns whatever columns the test declares, not what the SQL
// selects. This test reads the source instead, so it cannot be fooled by a
// mock: it parses every non-test file in this package, finds each function
// that calls scanTask, and counts the select list of the task query in it.
//
// It publishes its denominator: a scanTask caller whose query it cannot find
// fails the test rather than passing as "examined and clean".
func TestTaskSelectArityMatchesScanTask(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	want := -1
	type caller struct {
		name string
		lits []string
	}
	var callers []caller

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == "scanTask" && fn.Recv == nil {
				want = scanArgCount(fn)
				continue
			}
			calls := false
			var lits []string
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "scanTask" {
						calls = true
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						if s, err := strconv.Unquote(x.Value); err == nil {
							lits = append(lits, s)
						}
					}
				}
				return true
			})
			if calls {
				callers = append(callers, caller{name: fn.Name.Name, lits: lits})
			}
		}
	}

	if want <= 0 {
		t.Fatalf("could not find scanTask's Scan destinations (got %d)", want)
	}
	if len(callers) == 0 {
		t.Fatal("found no scanTask callers; the guard examined nothing")
	}

	// The select list of a task query starts at "SELECT [alias.]id, [alias.]project_id"
	// (or "RETURNING id, project_id") and ends at FROM / the end of the literal.
	listRE := regexp.MustCompile(`(?is)\b(?:SELECT|RETURNING)\s+((?:\w+\.)?id\s*,\s*(?:\w+\.)?project_id\s*,.*?)(?:\bFROM\b|$)`)
	examined := 0
	for _, c := range callers {
		found := 0
		for _, lit := range c.lits {
			for _, m := range listRE.FindAllStringSubmatch(lit, -1) {
				found++
				got := len(strings.Split(strings.TrimSpace(strings.TrimRight(strings.TrimSpace(m[1]), ";")), ","))
				if got != want {
					t.Errorf("%s: task query selects %d columns, scanTask reads %d", c.name, got, want)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s calls scanTask but no task select list was found in it; extend this guard", c.name)
		}
		examined += found
	}
	t.Logf("examined %d task select lists across %d scanTask callers against %d destinations", examined, len(callers), want)
}

// scanArgCount returns the number of arguments of the single Scan call in fn.
func scanArgCount(fn *ast.FuncDecl) int {
	n := -1
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Scan" {
			n = len(call.Args)
		}
		return true
	})
	return n
}

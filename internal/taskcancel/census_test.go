package taskcancel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// 05-scheduler §4.11, amendment D3 (GitHub #304, 2026-10-08): every non-test
// write of CANCELLED to a task row goes through taskcancel.Cancel, so the
// parent unblock and the child cascade cannot be skipped by a site nobody
// listed (#300 found four sites without teardown; #304 seven more without the
// parent unblock, and one site the audit table had missed).
//
// The census matches the write shapes this codebase uses: a call to
// TransitionToCancelled; a TransitionConditional or UpdateStatus call whose
// target is persistence.TaskStatusCancelled; a Status field assigned
// persistence.TaskStatusCancelled; and a string literal holding
// "UPDATE tasks" that sets status = 'CANCELLED' (a WHERE status IN (...,
// 'CANCELLED') is a read of the status, not a write). It does NOT see a target computed at run
// time (a variable) or SQL assembled from fragments; it prints how many files
// and writes it examined so a census that matched nothing is visible.

// censusAllowed are the writes that are not a cancel site: the primitive
// itself, the repository drivers that implement the write, and the
// executor's own sites #8-#11 (the cascade, cancelDescendants behind
// CancelChildren, and the dependents sweep are the
// primitive's post-write steps; executor.Cancel is CancelIfActive's own
// write; handleCancelled is the execution ending). Keyed by the path from the
// repository root, then the enclosing function ("*" for the whole file).
var censusAllowed = map[string]map[string]bool{
	"internal/taskcancel/taskcancel.go":                {"*": true},
	"internal/persistence/sqlite/task_repository.go":   {"*": true},
	"internal/persistence/postgres/task_repository.go": {"*": true},
	"internal/persistence/mocks/task_repository.go":    {"*": true},
	"internal/persistence/repotest/repotest.go":        {"*": true},
	"internal/executor/executor.go":                    {"Cancel": true},
	"internal/executor/workflow.go":                    {"handleCancelled": true, "cancelDescendants": true, "cancelBlockedDependents": true},
}

// setCancelledSQL is a SET of the status column to CANCELLED.
var setCancelledSQL = regexp.MustCompile(`(?i)\bset\s+status\s*=\s*'CANCELLED'`)

type cancelWrite struct {
	file, fn string
	line     int
	shape    string
}

func isCancelledConst(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name == "TaskStatusCancelled"
	case *ast.Ident:
		return v.Name == "TaskStatusCancelled"
	}
	return false
}

// findCancelWrites returns every CANCELLED task write in one parsed file.
func findCancelWrites(fset *token.FileSet, rel string, f *ast.File) []cancelWrite {
	var out []cancelWrite
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fn := fd.Name.Name
		add := func(n ast.Node, shape string) {
			out = append(out, cancelWrite{file: rel, fn: fn, line: fset.Position(n.Pos()).Line, shape: shape})
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "TransitionToCancelled":
					add(v, "TransitionToCancelled")
				case "TransitionConditional":
					if len(v.Args) >= 4 && isCancelledConst(v.Args[3]) {
						add(v, "TransitionConditional(CANCELLED)")
					}
				case "UpdateStatus":
					if len(v.Args) >= 3 && isCancelledConst(v.Args[2]) {
						add(v, "UpdateStatus(CANCELLED)")
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range v.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Status" && i < len(v.Rhs) && isCancelledConst(v.Rhs[i]) {
						add(v, "Status = CANCELLED")
					}
				}
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					s, err := strconv.Unquote(v.Value)
					if err == nil && strings.Contains(s, "UPDATE tasks") && setCancelledSQL.MatchString(s) {
						add(v, "SQL UPDATE tasks ... 'CANCELLED'")
					}
				}
			}
			return true
		})
	}
	return out
}

func TestCancelledWritesGoThroughThePrimitive(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	files, writes := 0, 0
	var stray []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if repowalk.SkipDir(filepath.Join(root, dir), path, d.Name(), "testdata") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			files++
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, w := range findCancelWrites(fset, rel, f) {
				writes++
				if allow := censusAllowed[w.file]; allow["*"] || allow[w.fn] {
					continue
				}
				stray = append(stray, w.file+":"+strconv.Itoa(w.line)+" "+w.fn+" ("+w.shape+")")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	sort.Strings(stray)
	for _, s := range stray {
		t.Errorf("CANCELLED task write outside taskcancel.Cancel: %s", s)
	}
	t.Logf("examined %d files, %d CANCELLED task writes (%d outside the allowlist)", files, writes, len(stray))
	if files < 100 || writes < 5 {
		t.Fatalf("the census examined %d files and found %d writes: it is not looking where the writes are", files, writes)
	}
}

// The census can fail: each shape it claims to match is found in a source
// that holds it.
func TestCancelledWritesCensus_MatchesEveryShape(t *testing.T) {
	src := `package x
func a() { repo.TransitionToCancelled(ctx, id) }
func b() { repo.TransitionConditional(ctx, id, from, persistence.TaskStatusCancelled, opts) }
func c() { repo.UpdateStatus(ctx, id, persistence.TaskStatusCancelled) }
func d() { task.Status = persistence.TaskStatusCancelled }
func e() { q := "UPDATE tasks SET status = 'CANCELLED' WHERE id = ?"; _ = q }
func h() { q := "UPDATE tasks SET lease_id = NULL WHERE status IN ('COMPLETED', 'CANCELLED')"; _ = q }
func f() { repo.TransitionConditional(ctx, id, from, to, opts) }
func g() { exec.UpdateStatus(ctx, id, persistence.ExecutionStatusCancelled) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, w := range findCancelWrites(fset, "x.go", f) {
		got[w.fn] = true
	}
	for _, fn := range []string{"a", "b", "c", "d", "e"} {
		if !got[fn] {
			t.Errorf("shape in %s() not matched", fn)
		}
	}
	for _, fn := range []string{"f", "g", "h"} {
		if got[fn] {
			t.Errorf("%s() is not a CANCELLED task write but was matched", fn)
		}
	}
}

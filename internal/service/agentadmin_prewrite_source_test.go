package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"vornik.io/vornik/internal/controlplane"
)

// audit 2026-10-12: finding A (review f2c1 A-1). applyAndVerify Aborts a
// config account transition only for the ApplyEngine.Apply sentinels in
// preWriteApplyErrors, a claim about where they are returned: every return
// site must precede Apply's first file write. This pins it, so a new return
// site of a listed sentinel (or a new function returning one) fails here
// instead of turning an Abort after a half-landed bundle into a silent
// state. The denominator is printed: every return statement in the
// controlplane package that names a listed sentinel.
func TestSource_PreWriteApplyErrorsReturnBeforeTheFirstWrite(t *testing.T) {
	known := map[error]string{
		controlplane.ErrApplyInProgress: "ErrApplyInProgress", controlplane.ErrBusy: "ErrBusy",
		controlplane.ErrDaemonAckRequired: "ErrDaemonAckRequired", controlplane.ErrReviewOnly: "ErrReviewOnly",
		controlplane.ErrTooManyOps: "ErrTooManyOps", controlplane.ErrContentTooLarge: "ErrContentTooLarge",
		controlplane.ErrScaffoldConflict: "ErrScaffoldConflict",
	}
	names := map[string]bool{}
	for _, e := range preWriteApplyErrors {
		n, ok := known[e]
		if !ok {
			t.Fatalf("preWriteApplyErrors holds %v, which this guard does not know: add it here after checking its return sites", e)
		}
		names[n] = true
	}
	_, here, _, _ := runtime.Caller(0)
	paths, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(here)), "controlplane", "*.go"))
	fset := token.NewFileSet()
	examined := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			firstWrite := token.NoPos
			if isApplyEngineApply(fn) {
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "applyJournaled" && firstWrite == token.NoPos {
						firstWrite = sel.Pos()
					}
					return true
				})
				if firstWrite == token.NoPos {
					t.Fatal("ApplyEngine.Apply no longer calls applyJournaled: re-derive its first write before trusting preWriteApplyErrors")
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				var named string
				ast.Inspect(ret, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && names[id.Name] {
						named = id.Name
					}
					return true
				})
				if named == "" {
					return true
				}
				examined++
				where := fset.Position(ret.Pos())
				switch {
				case isApplyEngineApply(fn) && ret.Pos() < firstWrite:
				case fn.Name.Name == "Rollback" || fn.Name.Name == "Reconcile" || fn.Name.Name == "refuseBusy":
					// Not reached from Apply: Rollback, Reconcile and ResolveJournal (refuseBusy) are their own verbs.
				default:
					t.Errorf("%s returned at %s:%d in %s, not before Apply's first write: remove it from preWriteApplyErrors or move the return",
						named, filepath.Base(where.Filename), where.Line, fn.Name.Name)
				}
				return true
			})
		}
	}
	if examined == 0 {
		t.Fatal("no return site of a pre-write sentinel was found: the guard examined nothing")
	}
	t.Logf("examined %d return sites of %d pre-write sentinels", examined, len(names))
}

// isApplyEngineApply reports whether fn is (*ApplyEngine).Apply.
func isApplyEngineApply(fn *ast.FuncDecl) bool {
	if fn.Name.Name != "Apply" || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "ApplyEngine"
}

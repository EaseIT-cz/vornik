package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #294 (2026-10-08), process-spawn-law design "Change 2026-10-08 — no
// request reads container logs": `podman logs` runs in exactly one place, the
// executor's step-failure capture inside executeAgentStep, which runs in an
// execution the scheduler leased, never on a request. Before #294 a second
// site, Executor.TaskLogs, served GET .../logs, .../explain, the support
// report, the UI log panel and the UI post-mortem.
//
// The law: every non-test Go file in the module is parsed, and every call of
// a method named Logs on a receiver whose last selector is `runtime` (the
// executor's runtime manager, `e.runtime.Logs`) is listed with its enclosing
// function. Exactly one is allowed, in internal/executor's executeAgentStep. A
// new accessor that reads container logs adds a site and fails here, so it
// has to come through design review.
//
// What it does NOT cover: a runtime manager held under another field name, or
// a direct `podman logs` argv built outside internal/runtime (the spawn law's
// PodmanControl grammar and importer law own that). It reports how many files
// it examined, so a scan that silently matched nothing is visible.

type containerLogSite struct {
	file, fn string
	line     int
}

var allowedContainerLogSites = map[string]bool{
	"internal/executor executeAgentStep": true,
}

func TestSpawnLaw_ContainerLogsReadOnlyOnStepFailure(t *testing.T) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(out)))

	files := 0
	var sites []containerLogSite
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Hidden directories are never this module's source: .git, .claude
			// worktrees, and the CE export trees (.vornik-export,
			// .vornik-public-clone) that hold OLDER copies of the code and
			// failed this law on the main checkout (2026-10-08 merge).
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			switch d.Name() {
			case "vendor", "node_modules", "testdata", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		files++
		sites = append(sites, containerLogCalls(t, path, filepath.ToSlash(rel))...)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if files < 100 {
		t.Fatalf("examined only %d Go files under %s: the scan is not looking at the module", files, root)
	}
	t.Logf("examined %d non-test Go files; %d runtime Logs call site(s)", files, len(sites))

	seen := map[string]bool{}
	for _, s := range sites {
		key := filepath.ToSlash(filepath.Dir(s.file)) + " " + s.fn
		if !allowedContainerLogSites[key] {
			t.Errorf("%s:%d (%s) reads container logs through the runtime: only the executor's step-failure "+
				"capture may (GitHub #294; process-spawn-law design, Change 2026-10-08). Serve daemon-held "+
				"state (api.HeldTaskLog) instead", s.file, s.line, s.fn)
		}
		seen[key] = true
	}
	for key := range allowedContainerLogSites {
		if !seen[key] {
			t.Errorf("allowed site %q no longer reads container logs: the step-failure tail is gone or moved; "+
				"update the law and the design together", key)
		}
	}
}

// containerLogCalls returns every `<...>.runtime.Logs(...)` call in one file
// with its enclosing function name.
func containerLogCalls(t *testing.T, path, rel string) []containerLogSite {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var out []containerLogSite
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Logs" {
				return true
			}
			if isRuntimeReceiver(sel.X) {
				out = append(out, containerLogSite{file: rel, fn: fd.Name.Name, line: fset.Position(call.Pos()).Line})
			}
			return true
		})
	}
	return out
}

func isRuntimeReceiver(x ast.Expr) bool {
	switch r := x.(type) {
	case *ast.SelectorExpr:
		return r.Sel.Name == "runtime"
	case *ast.Ident:
		return r.Name == "runtime"
	}
	return false
}

func TestContainerLogCalls_FindsAPlantedAccessor(t *testing.T) {
	dir := t.TempDir()
	src := `package p
type rt interface{ Logs(a, b string, n int) (string, error) }
type E struct{ runtime rt }
func (e *E) TaskLogs(id string) (string, error) { return e.runtime.Logs("", id, 1) }
func (e *E) other() { _ = e.Logs }
func (e *E) Logs() {}
`
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	sites := containerLogCalls(t, path, "internal/p/p.go")
	if len(sites) != 1 || sites[0].fn != "TaskLogs" {
		t.Fatalf("want the one planted TaskLogs site, got %+v", sites)
	}
}

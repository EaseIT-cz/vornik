package architecture

// Test-environment laws (testing design 19, section 11.3; GitHub #357).
//
// A developer shell can export a production daemon's VORNIK_* environment; CI
// exports none. A test that hands that environment to a daemon it starts, or
// builds a daemon in-process under it, works against production state on that
// host and against nothing in CI. Incident 2026-10-13 (#357): an e2e daemon
// inherited VORNIK_DATA_DIR, resolved production's workspace root and pruned
// live task worktrees; ordinary unit tests rewrote production's composed git
// config. These laws keep every such test on internal/testutil/testenv.

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// testEnvTagSets are the build-tag sets a daemon-building test can hide under.
var testEnvTagSets = []string{"", "e2e", "e2e_http", "e2e_hermes", "e2e_podman", "e2e_migrate", "integration"}

func testEnvModule(t *testing.T, root string) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// L1: every test binary that links internal/service (where the daemon's
// container and its data-dir writers live), and every test binary under
// test/e2e and test/integration (which start processes), has a TestMain that
// calls testenv.Main, under every tag set. Tag-aware: the TestMain must be in
// a file compiled into THAT binary.
func TestTestEnvLaw_TestBinariesRunIsolated(t *testing.T) {
	root := moduleRoot(t)
	module := testEnvModule(t, root)
	service := module + "/internal/service"
	type binary struct{ tags, pkg string }
	var need []binary
	files := map[binary][]string{} // the _test.go files compiled into the binary
	for _, tags := range testEnvTagSets {
		args := []string{"list", "-e", "-test", "-f",
			"{{.ImportPath}}|{{.Dir}}|{{join .TestGoFiles \",\"}}|{{join .XTestGoFiles \",\"}}|{{join .Deps \" \"}}"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		args = append(args, "./...")
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -tags %q: %v", tags, err)
		}
		testFiles := map[string][]string{}
		var binaries []string
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			parts := strings.SplitN(sc.Text(), "|", 5)
			if len(parts) != 5 {
				continue
			}
			imp, dir := parts[0], parts[1]
			if strings.HasSuffix(imp, ".test") {
				pkg := strings.TrimSuffix(imp, ".test")
				rel := strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
				if containsField(parts[4], service) || strings.HasPrefix(rel, "test/e2e") || strings.HasPrefix(rel, "test/integration") {
					binaries = append(binaries, pkg)
				}
				continue
			}
			if strings.Contains(imp, " ") { // "p [p.test]" variants
				continue
			}
			for _, f := range append(strings.Split(parts[2], ","), strings.Split(parts[3], ",")...) {
				if f != "" {
					testFiles[imp] = append(testFiles[imp], filepath.Join(dir, f))
				}
			}
		}
		for _, pkg := range binaries {
			b := binary{tags, strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")}
			need = append(need, b)
			files[b] = testFiles[pkg]
		}
	}
	if len(need) == 0 {
		t.Fatal("no test binary in scope: the derivation is broken, not clean")
	}
	sort.Slice(need, func(i, j int) bool {
		if need[i].pkg != need[j].pkg {
			return need[i].pkg < need[j].pkg
		}
		return need[i].tags < need[j].tags
	})
	var names []string
	for _, b := range need {
		names = append(names, b.pkg+"["+b.tags+"]")
	}
	t.Logf("examined %d (package, tag set) test binaries across %d tag sets: %v", len(need), len(testEnvTagSets), names)
	for _, b := range need {
		if !filesHaveTestMainCalling(t, files[b], "testenv", "Main") {
			t.Errorf("%s (tags %q): no TestMain in this binary calls testenv.Main — "+
				"add `func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }` (testing design 19 §11.2)", b.pkg, b.tags)
		}
	}
}

func filesHaveTestMainCalling(t *testing.T, files []string, pkgName, fn string) bool {
	t.Helper()
	for _, f := range files {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != "TestMain" || fd.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkgName && sel.Sel.Name == fn {
						found = true
					}
				}
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}

// L2: a test that starts a daemon (or anything it built) gives it an explicit,
// allowlisted environment. Scope: every _test.go under test/e2e and
// test/integration, plus any _test.go that `go build`s a ./cmd/vornik* main.
func TestTestEnvLaw_StartedProcessesGetAnExplicitEnvironment(t *testing.T) {
	root := moduleRoot(t)
	var scanned, inScope, sites int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		scanned++
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not this law's business; the compiler reports it
		}
		if !strings.HasPrefix(rel, "test/e2e/") && !strings.HasPrefix(rel, "test/integration/") && !buildsDaemonMain(file) {
			return nil
		}
		inScope++
		sites += checkStartedProcessEnv(t, fset, rel, file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scanned %d _test.go files; %d in scope; %d exec sites with a non-literal program examined", scanned, inScope, sites)
	// sites == 0 is a canary, not a correctness check: today the e2e lanes start
	// the binaries they build, so zero sites means the scan broke. If that ever
	// stops being true, retire the canary rather than the law.
	if inScope == 0 || sites == 0 {
		t.Fatal("nothing in scope: the scope derivation is broken, not clean")
	}
}

// buildsDaemonMain: a call carrying both a "build" literal and a
// "./cmd/vornik…" literal (go build of a daemon or CLI main).
func buildsDaemonMain(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		var build, main bool
		for _, a := range call.Args {
			if s, ok := stringLit(a); ok {
				build = build || s == "build"
				main = main || strings.HasPrefix(s, "./cmd/vornik")
			}
		}
		found = build && main
		return !found
	})
	return found
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

func isExecCommand(call *ast.CallExpr) (progArg int, ok bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "exec" {
		return 0, false
	}
	switch sel.Sel.Name {
	case "Command":
		return 0, true
	case "CommandContext":
		return 1, true
	}
	return 0, false
}

// checkStartedProcessEnv reports, per function: any os.Environ() call, and any
// exec.Command(Context) with a non-literal program that is not assigned to a
// variable whose .Env the same function assigns. Returns the number of
// non-literal exec sites examined.
func checkStartedProcessEnv(t *testing.T, fset *token.FileSet, rel string, file *ast.File) int {
	t.Helper()
	sites := 0
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Environ" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" {
					t.Errorf("%s:%d: os.Environ() hands the shell's VORNIK_* (production's, on a developer host) to a child; use testenv.Clean or testenv.Daemon (testing design 19 §11)",
						rel, fset.Position(call.Pos()).Line)
				}
			}
		}
		return true
	})
	var funcs []*ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		switch f := n.(type) {
		case *ast.FuncDecl:
			if f.Body != nil {
				funcs = append(funcs, f.Body)
			}
		case *ast.FuncLit:
			funcs = append(funcs, f.Body)
		}
		return true
	})
	for _, body := range funcs {
		// Variables whose .Env this function (not a nested literal) assigns.
		envSet := map[string]bool{}
		assigned := map[*ast.CallExpr]string{}
		inspectShallow(body, func(n ast.Node) {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return
			}
			for i, lhs := range as.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
					if id, ok := sel.X.(*ast.Ident); ok {
						envSet[id.Name] = true
					}
				}
				if i < len(as.Rhs) {
					if call, ok := as.Rhs[i].(*ast.CallExpr); ok {
						if id, ok := lhs.(*ast.Ident); ok {
							assigned[call] = id.Name
						}
					}
				}
			}
		})
		inspectShallow(body, func(n ast.Node) {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return
			}
			prog, ok := isExecCommand(call)
			if !ok || prog >= len(call.Args) {
				return
			}
			if _, lit := stringLit(call.Args[prog]); lit {
				return
			}
			sites++
			v, ok := assigned[call]
			if !ok || !envSet[v] {
				t.Errorf("%s:%d: a test-started program (non-literal exec.Command) runs with no explicit Env, so it inherits the shell's VORNIK_*; assign <cmd>.Env = testenv.Clean(…)/testenv.Daemon(…) (testing design 19 §11)",
					rel, fset.Position(call.Pos()).Line)
			}
		})
	}
	return sites
}

// inspectShallow visits a function body without descending into nested
// function literals (each is checked as its own function).
func inspectShallow(body *ast.BlockStmt, visit func(ast.Node)) {
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if n != nil {
			visit(n)
		}
		return true
	})
}

// L3: the sentinel. Build a daemon container in a real `go test` subprocess
// with VORNIK_DATA_DIR pointing at an empty sentinel directory (every other
// VORNIK_* removed, as CI has them) and require the sentinel to stay empty.
// Before the fix the run left <sentinel>/git/config (the daemon's composed git
// config), which on a developer host is production's file (#357).
func TestTestEnvLaw_SentinelDataDirUntouchedByServiceTests(t *testing.T) {
	root := moduleRoot(t)
	if _, err := os.Stat(filepath.Join(root, "internal", "service")); err != nil {
		// #334 item 6 (amendment 2026-10-14): only the Community export may lack it.
		absentFromEdition(t, root, "internal/service")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed: the composer cannot run, so the sentinel proves nothing")
	}
	sentinel := t.TempDir()
	t.Logf("examined: 1 test (TestNewContainer_ApplyOptionsBeforeInitLogship_OrderingRegression) of internal/service, " +
		"under an inherited VORNIK_DATA_DIR that its testenv.Main must redirect; L1 covers the other binaries structurally")
	cmd := exec.Command("go", "test", "-count=1",
		"-run", "^TestNewContainer_ApplyOptionsBeforeInitLogship_OrderingRegression$", "./internal/service")
	cmd.Dir = root
	env := []string{"VORNIK_DATA_DIR=" + sentinel}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "VORNIK_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test ./internal/service: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("ok")) {
		t.Fatalf("the sentinel run did not run the test:\n%s", out)
	}
	var left []string
	_ = filepath.WalkDir(sentinel, func(p string, _ os.DirEntry, err error) error {
		if err == nil && p != sentinel {
			rel, _ := filepath.Rel(sentinel, p)
			left = append(left, rel)
		}
		return nil
	})
	if len(left) > 0 {
		t.Fatalf("a unit test wrote into the inherited VORNIK_DATA_DIR %s: %v — on a developer host that is production's data dir (#357)", sentinel, left)
	}
}

// L4: only tests import testenv.
func TestTestEnvLaw_OnlyTestsImportTestenv(t *testing.T) {
	root := moduleRoot(t)
	module := testEnvModule(t, root)
	target := strconv.Quote(module + "/internal/testutil/testenv")
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/testutil/") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil
		}
		scanned++ // counted only once parsed: an unparsed file was not examined (#334 item 6)
		for _, imp := range file.Imports {
			if imp.Path.Value == target {
				t.Errorf("%s imports internal/testutil/testenv: it is for tests only", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	requireExamined(t, "testenv import law", "non-test Go files", scanned, 100)
}

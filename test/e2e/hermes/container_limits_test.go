package hermes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// limitBuilder is the one function allowed to put "run" at the head of a
// podman argv; it adds --memory and --memory-swap (lane design, "Memory
// guardrails", 2026-10-14).
const limitBuilder = "podmanRunArgs"

// podmanRunScan is what the scan saw: the denominator and the violations.
type podmanRunScan struct {
	Files         int      // .go files parsed
	PodmanCalls   int      // calls with a "podman" string argument
	BuilderUses   int      // calls to limitBuilder
	Violations    []string // file:line of a podman run built elsewhere
	ConfigLimited bool     // the lane's daemon config sets agent_memory_limit
}

func strLit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}

// formatsAgentLimit reports whether call formats a config whose runtime block
// sets agent_memory_limit, with laneAgentMemoryLimit among its arguments.
func formatsAgentLimit(call *ast.CallExpr) bool {
	var tmpl, arg bool
	for _, a := range call.Args {
		if s, ok := strLit(a); ok && strings.Contains(s, "\nruntime:\n") && strings.Contains(s, "\n  agent_memory_limit: %q\n") {
			tmpl = true
		}
		if id, ok := a.(*ast.Ident); ok && id.Name == "laneAgentMemoryLimit" {
			arg = true
		}
	}
	return tmpl && arg
}

// scanPodmanRuns parses every .go file in dir, whatever its build tags, and
// reports each podman run argv not built by limitBuilder: a call whose
// arguments go "podman", "run", or a literal list of two or more elements
// starting "run".
func scanPodmanRuns(t *testing.T, dir string) podmanRunScan {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var sc podmanRunScan
	fset := token.NewFileSet()
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		sc.Files++
		exempt := map[*ast.CompositeLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if fd, ok := n.(*ast.FuncDecl); ok && fd.Name.Name == limitBuilder && fd.Recv == nil {
				return false // the builder itself
			}
			switch x := n.(type) {
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == limitBuilder || id.Name == "hermesRunArgs") {
					sc.BuilderUses++ // hermesRunArgs is podmanRunArgs after a preflight
					// A literal non-positive limit is an unbounded container
					// (review dc82 F3b); podmanRunArgs also panics on it.
					if id.Name == limitBuilder && len(x.Args) > 0 {
						if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.INT && strings.Trim(lit.Value, "0_") == "" {
							sc.Violations = append(sc.Violations, fset.Position(x.Pos()).String()+": "+limitBuilder+" with a zero limit")
						}
						if _, ok := x.Args[0].(*ast.UnaryExpr); ok {
							sc.Violations = append(sc.Violations, fset.Position(x.Pos()).String()+": "+limitBuilder+" with a negative limit")
						}
					}
				}
				if formatsAgentLimit(x) {
					sc.ConfigLimited = true
				}
				for i, a := range x.Args {
					if s, ok := strLit(a); ok && s == "podman" {
						sc.PodmanCalls++
						if i+1 < len(x.Args) {
							if next, ok := strLit(x.Args[i+1]); ok && next == "run" {
								sc.Violations = append(sc.Violations, fset.Position(x.Pos()).String()+": podman run argv not built by "+limitBuilder)
							}
						}
						break
					}
				}
			case *ast.KeyValueExpr:
				// A map or struct value such as a JSON schema's
				// "required": []string{"run"} is data, not an argv.
				if cl, ok := x.Value.(*ast.CompositeLit); ok {
					exempt[cl] = true
				}
			case *ast.CompositeLit:
				// Any other list starting "run", whatever follows (an
				// append([]string{"run"}, …) is the same argv). A false
				// positive fails loudly, which is the safe direction.
				if len(x.Elts) >= 1 && !exempt[x] {
					if first, ok := strLit(x.Elts[0]); ok && first == "run" {
						sc.Violations = append(sc.Violations, fset.Position(x.Pos()).String()+": a run argv literal not built by "+limitBuilder)
					}
				}
			}
			return true
		})
	}
	return sc
}

// TestEveryLaneContainerCarriesAMemoryLimit is the census gate (incident
// 2026-10-14, found at: production: the lane's llama.cpp ran with
// HostConfig.Memory=0 and the OOM killer took a production container with
// it). A podman run added to this package without going through
// podmanRunArgs fails here. The denominator is printed so that a scan which
// matched nothing cannot read as clean (ENGINEERING-TENETS §7.4).
func TestEveryLaneContainerCarriesAMemoryLimit(t *testing.T) {
	sc := scanPodmanRuns(t, ".")
	t.Logf("examined %d files, %d podman call sites, %d %s/hermesRunArgs uses; %d violations",
		sc.Files, sc.PodmanCalls, sc.BuilderUses, limitBuilder, len(sc.Violations))
	for _, v := range sc.Violations {
		t.Error(v)
	}
	if sc.Files == 0 || sc.PodmanCalls == 0 || sc.BuilderUses == 0 {
		t.Fatalf("the scan examined nothing it could judge (files %d, podman calls %d, builder uses %d)", sc.Files, sc.PodmanCalls, sc.BuilderUses)
	}
	if !sc.ConfigLimited {
		t.Error("the lane's daemon config must set runtime.agent_memory_limit from laneAgentMemoryLimit: unset, the daemon derives about half the host per agent container")
	}
}

// The scan must go red on each shape it guards (ENGINEERING-TENETS §7.1).
func TestPodmanRunScan_CatchesEachShape(t *testing.T) {
	cases := map[string]string{
		"direct call":  `exec.Command("podman", "run", "--rm", "img")`,
		"argv literal": `args := []string{"run", "-d", "--name", n}`,
		"helper call":  `run(t, "podman", "run", "-d", "img")`,
		"appended":     `args := append([]string{"run"}, "--rm", "img")`,
		"zero limit":   `_ = ` + limitBuilder + `(0, "img")`,
		"neg limit":    `_ = ` + limitBuilder + `(-1, "img")`,
	}
	for name, stmt := range cases {
		dir := t.TempDir()
		src := "package p\n\nfunc f() {\n\t" + stmt + "\n\t_ = " + limitBuilder + "(1, \"x\")\n}\n"
		if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if sc := scanPodmanRuns(t, dir); len(sc.Violations) != 1 {
			t.Errorf("%s: want 1 violation, got %v", name, sc.Violations)
		}
	}
	// Not a podman argv: a JSON schema's required list (a map value), and
	// "run" later in a list.
	dir := t.TempDir()
	src := "package p\n\nvar r = map[string]any{\"required\": []string{\"run\"}}\nvar m = []string{\"first\", \"run\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if sc := scanPodmanRuns(t, dir); len(sc.Violations) != 0 {
		t.Errorf("false positive: %v", sc.Violations)
	}
	// The config check needs the runtime block's line AND the constant.
	for name, want := range map[string]bool{
		"limited":     true,
		"literal arg": false,
	} {
		arg := "laneAgentMemoryLimit"
		if !want {
			arg = `"1GiB"`
		}
		dir := t.TempDir()
		src := "package p\n\nimport \"fmt\"\n\nconst laneAgentMemoryLimit = \"1GiB\"\n\nvar c = fmt.Sprintf(\"x:\\nruntime:\\n  agent_memory_limit: %q\\n\", " + arg + ")\n"
		if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := scanPodmanRuns(t, dir).ConfigLimited; got != want {
			t.Errorf("%s: ConfigLimited = %v, want %v", name, got, want)
		}
	}
}

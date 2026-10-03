package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review 20261003-2ed0 B3: every agent container start goes through the
// model reach check. executeAgentStep is the one entry: it resolves the
// effective model, runs checkModelReach, and only then reaches the warm pool
// or startContainer. A warm container's env is baked when it first starts
// (warmPool.StartWarm), and a reused one keeps the model it started with,
// whatever the check judged now; so agent-namespace roles never use the warm
// pool (stepRuntimePolicy), and every start below stays inside executeAgentStep,
// after the check.

// stepRuntimePolicy: an agent project's role always starts its own container.
func TestStepRuntimePolicy_NeverWarmForAgentProjects(t *testing.T) {
	cases := []struct{ project, policy, want string }{
		{"hermes--fin", "warm", "ephemeral"},
		{"hermes--fin", "ephemeral", "ephemeral"},
		{"assistant", "warm", "warm"},
		{"assistant", "", ""},
	}
	for _, c := range cases {
		if got := stepRuntimePolicy(c.project, c.policy); got != c.want {
			t.Errorf("stepRuntimePolicy(%q, %q) = %q, want %q", c.project, c.policy, got, c.want)
		}
	}
}

// The start calls and where they sit: each is in executeAgentStep (or in a
// helper only it calls) and executeAgentStep calls checkModelReach before
// the first of them.
func TestSource_EveryAgentStartPassesTheModelCheck(t *testing.T) {
	starts := map[string]bool{"startContainer": true, "executeWarmAgentStep": true, "StartWarm": true, "StartContainer": true, "Acquire": true}
	allowed := map[string]bool{"executeAgentStep": true, "executeWarmAgentStep": true, "startContainer": true}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var checkPos, firstStart token.Pos
	examined := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if fn.Name.Name == "executeAgentStep" && sel.Sel.Name == "checkModelReach" && checkPos == 0 {
					checkPos = call.Pos()
				}
				if !starts[sel.Sel.Name] {
					return true
				}
				// Acquire and StartContainer are generic names; only the
				// warm pool's and the runtime's count.
				if x, ok := sel.X.(*ast.SelectorExpr); ok && (x.Sel.Name == "warmPool" || x.Sel.Name == "runtime") || sel.Sel.Name == "startContainer" || sel.Sel.Name == "executeWarmAgentStep" {
					examined++
					if !allowed[fn.Name.Name] {
						t.Errorf("%s calls %s outside executeAgentStep's checked path", fn.Name.Name, sel.Sel.Name)
					}
					if fn.Name.Name == "executeAgentStep" && (firstStart == 0 || call.Pos() < firstStart) {
						firstStart = call.Pos()
					}
				}
				return true
			})
		}
	}
	if examined == 0 {
		t.Fatal("no container start call was examined")
	}
	if checkPos == 0 || firstStart == 0 || checkPos > firstStart {
		t.Fatalf("executeAgentStep does not run checkModelReach before its first start (check at %v, start at %v)", fset.Position(checkPos), fset.Position(firstStart))
	}
	t.Logf("examined %d container start calls", examined)
}

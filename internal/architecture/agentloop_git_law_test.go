package architecture

// Process-spawn law, S6 review residual (design "S6 — git state an agent can
// write", S6-D3, round-2 G4): spawn is the ONLY daemon seam that starts git,
// with one exception — internal/agentloop imports os/exec for its git_* tools
// (spawnLawExceptions). That exception is sound only while those tools run
// INSIDE the agent sandbox. They are unexported and reachable only through
// agentloop.Dispatch / agentloop.Handlers, so this law pins who may reach
// those two entry points:
//
//   - cmd/agent-helper: the in-sandbox helper binary (it is not a daemon main;
//     spawnLawDaemonMains excludes it for that reason);
//   - internal/configassist: the daemon's config assistant, whose own Dispatch
//     refuses every git_* tool before agentloop sees it — pinned by
//     internal/configassist/envelope_test.go (a git_* tool "is IN the
//     envelope" fails there).
//
// Any other package selecting agentloop.Dispatch or agentloop.Handlers fails
// here, so a daemon path to agentloop's git cannot appear unreviewed.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const agentloopPath = "internal/agentloop"

// agentloopDispatchers: packages allowed to reach agentloop's tool dispatch.
var agentloopDispatchers = map[string]string{
	"cmd/agent-helper":      "runs inside the agent sandbox",
	"internal/configassist": "daemon config assistant; its Dispatch refuses git_* (configassist/envelope_test.go)",
}

// agentloopEntryPoints are the agentloop identifiers that reach a tool handler.
var agentloopEntryPoints = map[string]bool{"Dispatch": true, "Handlers": true}

func agentloopDispatchViolations(uses map[string]map[string]bool) []string {
	var out []string
	for pkg, ids := range uses {
		if _, ok := agentloopDispatchers[pkg]; ok {
			continue
		}
		for id := range ids {
			if agentloopEntryPoints[id] {
				out = append(out, pkg+" reaches agentloop."+id+": agentloop's git tools may run only inside the sandbox")
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestSpawnLaw_AgentloopGitRunsOnlyInTheSandbox_SelfTest(t *testing.T) {
	got := agentloopDispatchViolations(map[string]map[string]bool{
		"internal/api":          {"Dispatch": true},
		"internal/configassist": {"Dispatch": true, "Env": true},
		"internal/contractreg":  {"HandlerNames": true},
	})
	want := []string{"internal/api reaches agentloop.Dispatch: agentloop's git tools may run only inside the sandbox"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("self-test:\n got %q\nwant %q", got, want)
	}
}

func TestSpawnLaw_AgentloopGitRunsOnlyInTheSandbox(t *testing.T) {
	root, module := moduleRootAndPath(t)
	target := module + "/" + agentloopPath
	uses := map[string]map[string]bool{}
	examined, importers := 0, 0
	present := false
	for _, line := range depsGoList(t, root, "-f", `{{.ImportPath}}|{{.Dir}}|{{join .GoFiles " "}}|{{join .Imports " "}}`, "./...") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		rel, ok := strings.CutPrefix(parts[0], module+"/")
		if !ok {
			continue
		}
		examined++
		if rel == agentloopPath {
			present = true
			continue
		}
		if !containsField(parts[3], target) {
			continue
		}
		importers++
		ids := map[string]bool{}
		for _, f := range strings.Fields(parts[2]) {
			for id := range packageSelectors(t, filepath.Join(parts[1], f), target, "agentloop") {
				ids[id] = true
			}
		}
		uses[rel] = ids
	}
	if !present {
		t.Skip("internal/agentloop is absent from this edition")
	}
	t.Logf("examined %d packages; %d import internal/agentloop", examined, importers)
	for _, v := range agentloopDispatchViolations(uses) {
		t.Error(v)
	}
}

// packageSelectors returns the identifiers a file selects from importPath.
func packageSelectors(t *testing.T, file, importPath, defaultName string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	name := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == importPath {
			name = defaultName
			if imp.Name != nil {
				name = imp.Name.Name
			}
		}
	}
	out := map[string]bool{}
	if name == "" {
		return out
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
				out[sel.Sel.Name] = true
			}
		}
		return true
	})
	return out
}

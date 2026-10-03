package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Agent-administered design §18.6 item 2 in detail, round 3 F3 and review
// a125: agent_model_provider_approvals has two writers, named. Only the
// widening_change effect inserts a row (recordGrant, run by applyApproved);
// only the operator console sets removed_at. A new caller of either fails
// here until the design says otherwise.

// modelCallSites returns "<dir>/<file>:<enclosing func>" for every non-test
// call to a function or method named name under internal/.
func modelCallSites(t *testing.T, name string) []string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(here)) // internal/
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/repotest/") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
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
				switch f := call.Fun.(type) {
				case *ast.Ident:
					if f.Name == name {
						seen[rel+":"+fn.Name.Name] = true
					}
				case *ast.SelectorExpr:
					if f.Sel.Name == name {
						seen[rel+":"+fn.Name.Name] = true
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func pinModelCallers(t *testing.T, name string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := modelCallSites(t, name); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s is called from:\n  %s\nwant exactly:\n  %s", name, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Review 20261003-a525 A2: the apply-time destination check is its own
// function (it compares the device's destination with the live route; it is
// not the run-time refusal), and the widening effect runs it.
func TestSource_ApplyTimeCheckIsDistinct(t *testing.T) {
	pinModelCallers(t, "modelRouteMoved", "service/agentadmin_service.go:applyApproved")
	// Design §18.14 round 2 F6: list_my_setup's runs_on reuses the run-time
	// judgement (RoleRunsOn calls RunModelRefusal), so the setup and the
	// executor cannot disagree; only ListSetup reads it.
	pinModelCallers(t, "RunModelRefusal", "service/agentadmin_models.go:verifyAgentModel", "agentadmin/models.go:RoleRunsOn")
	pinModelCallers(t, "RoleRunsOn", "service/agentadmin_read.go:ListSetup")
}

// Review 20261003-a525 A3: the state's catalogue is BuildCatalogue's output
// and nothing else's. State.models is unexported and written only by
// State.UseModelCatalogue, which calls BuildCatalogue; the daemon reaches it only
// from loadStateExcluding.
func TestSource_StateModelsOnlyFromBuildCatalogue(t *testing.T) {
	pinModelCallers(t, "BuildCatalogue", "agentadmin/state.go:UseModelCatalogue", "service/agentadmin_models.go:agentModelCatalogue")
	pinModelCallers(t, "UseModelCatalogue", "service/agentadmin_service.go:loadStateExcluding")
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(here)) // internal/
	paths, _ := filepath.Glob(filepath.Join(root, "agentadmin", "*.go"))
	var sites []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "models" {
							sites = append(sites, filepath.Base(path)+":"+fn.Name.Name)
						}
					}
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok && k.Name == "models" {
						sites = append(sites, filepath.Base(path)+":"+fn.Name.Name+" (literal)")
					}
				}
				return true
			})
		}
	}
	if strings.Join(sites, ",") != "state.go:UseModelCatalogue" {
		t.Fatalf("State.models is written at %v; want only UseModelCatalogue", sites)
	}
}

// Review 20261003-a525 A6: this test asserts both writers.
func TestSource_ModelDestinationWriters(t *testing.T) {
	// Inserts: the widening_change effect only.
	pinModelCallers(t, "UpsertModelDestination", "service/agentadmin_service.go:recordGrant")
	// (mcpconnect has an unrelated recordGrant of its own; only this
	// package's is the agent admin effect's.)
	var effect []string
	for _, site := range modelCallSites(t, "recordGrant") {
		if strings.HasPrefix(site, "service/") {
			effect = append(effect, site)
		}
	}
	if strings.Join(effect, ",") != "service/agentadmin_cover.go:applyCover,service/agentadmin_service.go:applyApproved" {
		t.Errorf("the agent admin recordGrant is called from %v; want only the widening_change effect (applyApproved, applyCover)", effect)
	}
	// removed_at: the operator console only (/ui/admin/agents, behind the
	// admin session an agent key cannot reach).
	pinModelCallers(t, "MarkModelDestinationRemoved", "service/agentadmin_agents.go:WithdrawModelDestination")
	// lazyAgentsView forwards to agentsView; the console handler is the
	// only caller outside that pair.
	pinModelCallers(t, "WithdrawModelDestination", "service/agentadmin_agents.go:WithdrawModelDestination", "ui/admin_agents.go:AdminAgentWithdrawModel")
}

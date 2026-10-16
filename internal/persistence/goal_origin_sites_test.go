package persistence_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// GitHub #347 (learning-loop design, amendment 2026-10-13): the skill
// distiller judges a task's goal by tasks.goal_origin, so every task-creation
// site must stamp it, with a CONSTANT. This test is the denominator: it finds
// every creation literal in the module's non-test Go code and diffs it
// against the classified table in the design, both ways. A new site, a
// removed site or a changed constant fails here until the design's table and
// this list are updated together.
//
// A creation literal is a taskcreate.Params literal, or a persistence.Task
// literal that sets CreationSource (every row-creating literal does; the
// read-only stubs elsewhere do not). The persistence drivers and the repotest
// suite are test support, not creation paths, and are skipped.
var wantGoalOriginSites = map[string][]string{
	"internal/api/handlers.go":                          {"TaskGoalOriginREST", "TaskGoalOriginREST"},
	"internal/api/companion_mcp.go":                     {"TaskGoalOriginCompanion"},
	"internal/ui/project_create_task.go":                {"TaskGoalOriginUI"},
	"internal/autonomy/manager.go":                      {"TaskGoalOriginAutonomy"},
	"internal/service/projectdoctor_adapters.go":        {"TaskGoalOriginDoctor"},
	"internal/api/companion_broker.go":                  {"TaskGoalOriginBroker"},
	"internal/service/container_reminders.go":           {"TaskGoalOriginReminder"},
	"internal/api/webhook_handlers.go":                  {"TaskGoalOriginWebhook"},
	"internal/service/container_github_task_creator.go": {"TaskGoalOriginGitHub"},
	"internal/service/container_github_ci_outcomes.go":  {"TaskGoalOriginGitHubCI"},
	"internal/chat/parser.go":                           {"TaskGoalOriginChat"},
	"internal/service/container_a2a.go":                 {"TaskGoalOriginA2A"},
	"internal/executor/checkpoint.go":                   {"TaskGoalOriginCheckpoint"},
	"internal/executor/plan.go":                         {"TaskGoalOriginDelegation", "TaskGoalOriginRoute"},
	"internal/executor/spawn_project.go":                {"TaskGoalOriginSpawnProject"},
	"internal/executor/call_project.go":                 {"TaskGoalOriginCallProject"},
	"internal/replay/fork.go":                           {"TaskGoalOriginFork"},
	// Built with path.Join, not written as a quoted path: the public CE CI's
	// import backstop flags any line that starts with a quoted string naming
	// internal/enterprise (it failed the 2026.10.6 CE export on this row).
	path.Join("internal", "enterprise", "blackbox", "engine", "counterfactual.go"): {"TaskGoalOriginCounterfactual"},
	// The shared creator copies Params.GoalOrigin, which its callers set.
	"internal/taskcreate/creator.go": {"p.GoalOrigin"},
}

// TestGoalOrigin_UIIsStampedByOneSite: since the operator decision of
// 2026-10-13 (learning-loop LLD open question 6, GitHub #347) the ui origin
// GRANTS something, distillation without an API key, so it is the one origin
// whose stamping sites are a security boundary. A second ui site is a design
// change that needs its own review, not only an edit of the map above
// (review-20261013-daf4 F1).
func TestGoalOrigin_UIIsStampedByOneSite(t *testing.T) {
	var sites []string
	for file, consts := range wantGoalOriginSites {
		for _, c := range consts {
			if c == "TaskGoalOriginUI" {
				sites = append(sites, file)
			}
		}
	}
	if len(sites) != 1 || sites[0] != "internal/ui/project_create_task.go" {
		t.Fatalf("ui is stamped by %v; want exactly internal/ui/project_create_task.go", sites)
	}
}

func TestGoalOrigin_EveryCreationSiteStampsAConstant(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	skipPrefixes := []string{
		"internal/persistence/sqlite/", "internal/persistence/postgres/", "internal/persistence/repotest/",
	}
	got := map[string][]string{}
	fset := token.NewFileSet()
	literals := 0
	parsed := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, path, d.Name(), "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, p := range skipPrefixes {
			if strings.HasPrefix(rel, p) {
				return nil
			}
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// #334 item 6 (CI pipeline design, amendment 2026-10-14): a file
			// that does not parse was skipped as "not a creation site", so a
			// site in it read as absent. Build tags do not affect parsing, and
			// testdata is skipped above, so a parse error is a defect.
			t.Errorf("%s: does not parse (%v); the scan cannot see its creation sites", rel, perr)
			return nil
		}
		parsed++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			isParams := isSelector(lit.Type, "taskcreate", "Params") ||
				(f.Name.Name == "taskcreate" && isIdent(lit.Type, "Params"))
			isTask := isSelector(lit.Type, "persistence", "Task") ||
				(f.Name.Name == "persistence" && isIdent(lit.Type, "Task"))
			creates := isParams || (isTask && hasKey(lit, "CreationSource"))
			if !creates {
				return true
			}
			literals++
			pos := fset.Position(lit.Pos())
			v := keyValue(lit, "GoalOrigin")
			switch {
			case v == nil:
				t.Errorf("%s:%d: a task-creation literal without GoalOrigin; stamp a persistence.TaskGoalOrigin* constant and classify the site in the learning-loop design, amendment 2026-10-13", rel, pos.Line)
				got[rel] = append(got[rel], "<missing>")
			case isSelectorPrefix(v, "persistence", "TaskGoalOrigin"):
				got[rel] = append(got[rel], v.(*ast.SelectorExpr).Sel.Name)
			case rel == "internal/taskcreate/creator.go" && isSelector(v, "p", "GoalOrigin"):
				got[rel] = append(got[rel], "p.GoalOrigin")
			default:
				t.Errorf("%s:%d: GoalOrigin is not a persistence.TaskGoalOrigin* constant; a value copied from a request, payload or model can be forged by the text it classifies", rel, pos.Line)
				got[rel] = append(got[rel], "<not a constant>")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("goal-origin site scan: parsed %d non-test Go files, found %d task-creation literals", parsed, literals)

	for file := range got {
		sort.Strings(got[file])
	}
	want := map[string][]string{}
	wantCount := 0
	// The Community export prunes internal/enterprise as a whole, so its rows
	// are expected only when that tree is present (publish-ce, release
	// 2026.10.6). In the Enterprise tree a missing file still fails: the
	// rows are dropped only when the whole directory is absent.
	eeTree := true
	if _, statErr := os.Stat(filepath.Join(root, "internal", "enterprise")); statErr != nil {
		eeTree = false
	}
	dropped := 0
	for file, origins := range wantGoalOriginSites {
		if !eeTree && strings.HasPrefix(file, "internal/enterprise/") {
			dropped++
			continue
		}
		cp := append([]string(nil), origins...)
		sort.Strings(cp)
		want[file] = cp
		wantCount += len(cp)
	}
	if dropped > 0 {
		t.Logf("goal-origin site scan: internal/enterprise is absent (a Community tree); %d Enterprise row(s) of the table not expected", dropped)
	}
	for file, w := range want {
		if g := got[file]; strings.Join(g, ",") != strings.Join(w, ",") {
			t.Errorf("%s: creation sites stamp %v, the design's table says %v", file, g, w)
		}
	}
	for file, g := range got {
		if _, ok := want[file]; !ok {
			t.Errorf("%s: unclassified task-creation site(s) %v; add them to the table in the learning-loop design, amendment 2026-10-13, and to wantGoalOriginSites", file, g)
		}
	}
	if literals != wantCount {
		t.Errorf("found %d task-creation literals, the table lists %d", literals, wantCount)
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isSelector(e ast.Expr, pkg, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	return ok && isIdent(s.X, pkg) && s.Sel.Name == name
}

func isSelectorPrefix(e ast.Expr, pkg, prefix string) bool {
	s, ok := e.(*ast.SelectorExpr)
	return ok && isIdent(s.X, pkg) && strings.HasPrefix(s.Sel.Name, prefix)
}

func keyValue(lit *ast.CompositeLit, key string) ast.Expr {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok && isIdent(kv.Key, key) {
			return kv.Value
		}
	}
	return nil
}

func hasKey(lit *ast.CompositeLit, key string) bool { return keyValue(lit, key) != nil }

// The scan's directory rule is repowalk.SkipDir with testdata (testing LLD
// §12): every hidden directory below the root (.git, .claude worktrees and the
// CE export copies the gate's parity lane creates while this test runs, which
// counted every site three times on 3f0ed9052, 2026-10-13: 60 literals against
// a table of 21), vendor, node_modules and testdata; never the root.

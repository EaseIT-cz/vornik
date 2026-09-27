package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Only optional-work callers may mark a chat call best-effort (model health
// circuit breaker design §5.3a, 2026-09-25). A best-effort call's deadline is
// not a breaker sample, so a mark on TASK traffic would blind the breaker to a
// hanging model for that traffic, which is the failure the breaker exists for.
//
// The law is on call sites of chat.WithBestEffort, in non-test files, by
// package. Self-contained and module-agnostic (it ships in the Community
// export; see deps_install_law_test.go).

// bestEffortAllowed: package path prefixes (relative to the module) that may
// call chat.WithBestEffort, each with its reason. The executor, the chat
// proxy and every agent path must never appear here.
var bestEffortAllowed = map[string]string{
	"internal/narrator": "narration lines are decorative, under the narrator's own 10 s deadline",
	"internal/memory":   "titles, classes, narratives and reranks are optional enrichment under their own deadlines",
}

func TestOnlyOptionalWorkMarksChatCallsBestEffort(t *testing.T) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(out)))
	module := depsGoList(t, root, "-m")[0]
	chatPkg := module + "/internal/chat"

	callers := map[string]bool{}
	for _, line := range depsGoList(t, root, "-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .GoFiles \",\"}}", "./...") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[2] == "" || parts[0] == chatPkg {
			continue
		}
		rel := strings.TrimPrefix(parts[0], module+"/")
		for _, name := range strings.Split(parts[2], ",") {
			marks, sited := chatCallsIn(t, filepath.Join(parts[1], name), chatPkg)
			if marks && !sited {
				t.Errorf("%s/%s marks a call best-effort without chat.WithCallSite: vornik_chat_best_effort_deadline_total could not say which feature is starved", rel, name)
			}
			if marks {
				callers[rel] = true
				if !bestEffortPackageAllowed(rel) {
					t.Errorf("%s/%s calls chat.WithBestEffort: only optional-work packages may (breaker design §5.3a); task traffic must stay a health signal", rel, name)
				}
			}
		}
	}
	for prefix := range bestEffortAllowed {
		found := false
		for c := range callers {
			if c == prefix || strings.HasPrefix(c, prefix+"/") {
				found = true
			}
		}
		if !found {
			t.Errorf("allowlisted %s never calls chat.WithBestEffort: drop the entry, or the law is vacuous for it", prefix)
		}
	}
}

func bestEffortPackageAllowed(rel string) bool {
	for prefix := range bestEffortAllowed {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

// chatCallsIn reports whether the file calls chat.WithBestEffort and
// chat.WithCallSite, through an import of the chat package under whatever
// local name it is imported.
func chatCallsIn(t *testing.T, path, chatPkg string) (marks, sited bool) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	local := ""
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != chatPkg {
			continue
		}
		local = "chat"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" {
		return false, false
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != local {
			return true
		}
		switch sel.Sel.Name {
		case "WithBestEffort":
			marks = true
		case "WithCallSite":
			sited = true
		}
		return true
	})
	return marks, sited
}

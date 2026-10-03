package brokergrants_test

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

// Source tests for standing grants (broker write-actions design, tier 2
// revised item 10, round 3 F7, review 61a5 F1). Each pins the complete set
// of call sites of one sensitive function across internal/, so a second
// copy or a new caller fails here until the design says otherwise.

// callSites returns "<dir>/<file>:<enclosing func>" for every non-test call
// to a function or method named name.
func callSites(t *testing.T, name string) []string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(here)) // internal/
	var out []string
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
						out = append(out, rel+":"+fn.Name.Name)
					}
				case *ast.SelectorExpr:
					if f.Sel.Name == name {
						out = append(out, rel+":"+fn.Name.Name)
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
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(xs []string) []string {
	var out []string
	for i, x := range xs {
		if i == 0 || xs[i-1] != x {
			out = append(out, x)
		}
	}
	return out
}

func pin(t *testing.T, name string, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := callSites(t, name)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s is called from:\n  %s\nwant exactly:\n  %s", name, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Review 61a5 F1: one canonicalisation. Creation (ApproveWithGrant) and the
// decrement's key (Cover) both call KeyOf; Offer shows the action's own key.
func TestSource_OneCanonicalisation(t *testing.T) {
	pin(t, "KeyOf",
		"brokergrants/grants.go:ApproveWithGrant",
		"brokergrants/grants.go:Cover",
		"brokergrants/grants.go:Offer",
	)
}

// Round 3 F7: the opened key values exist only inside the matcher and the
// grant pages' view.
func TestSource_KeyValuesOpenedInTwoPlaces(t *testing.T) {
	pin(t, "openKey",
		"brokergrants/grants.go:Views",
		"brokergrants/grants.go:matchingGrant",
	)
}

// Item 10: grants are created only from the approver device's decision and
// the /inbox handler; no agent verb, API or companion tool reaches it.
func TestSource_GrantsCreatedOnlyByAPerson(t *testing.T) {
	pin(t, "ApproveWithGrant",
		"service/container_broker_grants.go:approveActionWithGrant",
		"ui/inbox_broker_grants.go:BrokerActionApproveWithGrant",
	)
	// The device's branch runs only as the broker_action effect, after the
	// device's own guarded decision.
	pin(t, "approveActionWithGrant", "service/agentadmin_actions.go:actionEffect")
	pin(t, "ApproveSeedAndCreate", "brokergrants/grants.go:ApproveWithGrant")
}

// Item 4: the only path to an approval under a grant.
func TestSource_OnePathToACoveredApproval(t *testing.T) {
	pin(t, "ApproveUnderGrant", "brokergrants/grants.go:Cover")
	pin(t, "BrokerGrantApprover",
		"persistence/postgres/broker_grant_repository.go:AdvanceDigest",
		"persistence/postgres/broker_grant_repository.go:ApproveUnderGrant",
		"persistence/postgres/broker_grant_repository.go:CoveredActions",
		"persistence/sqlite/broker_grant_repository.go:AdvanceDigest",
		"persistence/sqlite/broker_grant_repository.go:ApproveUnderGrant",
		"persistence/sqlite/broker_grant_repository.go:CoveredActions",
	)
}

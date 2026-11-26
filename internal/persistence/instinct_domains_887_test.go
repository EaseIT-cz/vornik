package persistence

// GitHub #887 (instinct tool-budget seam design, amendment 2026-11-23): the
// budget domain reached Postgres but not SQLite's instincts CHECK. Every
// domain constant must be in InstinctDomains(), which the driver tests insert,
// so a new domain cannot again reach one driver and not the other. Found at:
// unit, while writing the SQLite test of #886.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestInstinctDomainsListsEveryConstant_887(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "models.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, "InstinctDomain") || i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("%s is not a string literal", name.Name)
			}
			v, _ := strconv.Unquote(lit.Value)
			found = append(found, v)
		}
		return true
	})
	if len(found) < 6 {
		t.Fatalf("found %d InstinctDomain* constants in models.go, want at least 6: the parse is stale", len(found))
	}
	for _, d := range found {
		if !slices.Contains(InstinctDomains(), d) {
			t.Errorf("domain %q is a constant but not in InstinctDomains(): the drivers' tests would not insert it", d)
		}
	}
	if len(InstinctDomains()) != len(found) {
		t.Errorf("InstinctDomains() has %d entries, models.go %d constants", len(InstinctDomains()), len(found))
	}
}

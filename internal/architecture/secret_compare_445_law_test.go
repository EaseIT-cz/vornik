package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// GitHub #445 (pluggable-auth-design.md, "one length-safe secret
// comparison"): subtle.ConstantTimeCompare returns at once when lengths
// differ, so on a raw secret it tells a caller the secret's length by timing.
// These three files compare a presented secret with a stored one of variable
// length; each must go through apikey.SecretEqual, which compares fixed-length
// digests. Read from the syntax tree, so a comment naming the function is not
// a call.
// The broker's site is added by secret_compare_445_broker_law_test.go, which
// the Community export prunes with internal/broker (the 2026.11.3 release CI
// failed the CE export on this law parsing a file the export removes).
var secretCompareSites445 = []string{
	"internal/api/routes.go",
	"internal/api/credentials_rotate_582.go",
}

func TestSecretCompareSites_UseSecretEqual_445(t *testing.T) {
	root := moduleRoot(t)
	for _, rel := range secretCompareSites445 {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ConstantTimeCompare" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "subtle" {
				t.Errorf("%s: subtle.ConstantTimeCompare on a raw secret leaks its length; use apikey.SecretEqual", fset.Position(sel.Pos()))
			}
			return true
		})
	}
	t.Logf("secret-compare law: examined %d files", len(secretCompareSites445))
}

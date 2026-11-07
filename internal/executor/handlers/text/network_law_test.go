package text

// GitHub #633: registry.networkFreeSystemHandlers lists text.readability as a
// handler that cannot reach the network, which lets the companion catalogue
// keep calling the review workflows network-incapable. This test is what
// makes that entry true: it refuses every network import in this package and
// in internal/readability.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var networkImports = []string{
	"net", "net/http", "net/rpc", "net/smtp",
	"vornik.io/vornik/internal/netguard",
	"vornik.io/vornik/internal/mcp",
	"vornik.io/vornik/internal/forge",
	"vornik.io/vornik/internal/spawn",
}

func TestNetworkLaw_NoNetworkImports(t *testing.T) {
	examined := 0
	for _, dir := range []string{".", "../../../readability"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			examined++
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range networkImports {
					if path == bad || strings.HasPrefix(path, bad+"/") {
						t.Errorf("%s imports %s: text.readability is listed as network-free in registry.networkFreeSystemHandlers", f, path)
					}
				}
			}
		}
	}
	if examined < 3 {
		t.Fatalf("examined %d source files; the law must see this package and internal/readability", examined)
	}
}

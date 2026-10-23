package cli

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestCEIntegrationSkips_ListEveryImageGatedTest: every integration test in
// this package that calls agentImage(t) skips on a host without podman or the
// agent image, which the publish-ce runner is. The exported tree's integration
// lane fails on a skip the allow-list does not explain, so each such test needs
// a host-conditional (@<ci-job>) entry in scripts/ce-integration-skips.txt.
//
// Incident 2026-10-23: the 2026.10.7 publish-ce run refused to export because
// #428 added TestIntegrationDepsInstall_BoundedInstallSucceeds and
// TestIntegrationDepsInstall_KilledAtTheLimitSaysSo without entries. The local
// gate stamp's CE lane passed because the reference host has the image, so the
// two tests ran there.
func TestCEIntegrationSkips_ListEveryImageGatedTest(t *testing.T) {
	list := filepath.Join("..", "..", "scripts", "ce-integration-skips.txt")
	f, err := os.Open(list)
	if os.IsNotExist(err) {
		t.Skip("no CE skip allow-list in this tree; nothing to check it against")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	conditional := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) >= 3 && strings.HasPrefix(cols[1], "@") {
			conditional[cols[0]] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob("*_integration_test.go")
	if err != nil {
		t.Fatal(err)
	}
	var gated, missing []string
	fset := token.NewFileSet()
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") || !callsAgentImage(fn.Body) {
				continue
			}
			gated = append(gated, fn.Name.Name)
			if !conditional["vornik.io/vornik/internal/cli."+fn.Name.Name] {
				missing = append(missing, fn.Name.Name)
			}
		}
	}
	if len(gated) == 0 {
		t.Fatal("found no test calling agentImage(t); the scan is broken, not clean")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d of %d image-gated integration tests have no @<ci-job> entry in %s: %s",
			len(missing), len(gated), list, strings.Join(missing, ", "))
	}
}

func callsAgentImage(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "agentImage" {
				found = true
			}
		}
		return !found
	})
	return found
}

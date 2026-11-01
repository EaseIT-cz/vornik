package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// GitHub #566, SaaS MVP M7 (18-security-and-tenancy.md, "Amendment
// 2026-10-28 (#566, M7 daemon half)", E1): an image-declared loopback pair
// may not share a port with any TCP listener the daemon opens, and E1 names
// those listeners. This law keeps the list current: it counts the call sites
// of ListenAndServe, ListenAndServeTLS and net.Listen / net.ListenTCP with a
// constant tcp network, by syntax tree (comments never count), in non-test
// files. Four are the daemon's; the broker program's one is named and
// excluded; a sixth anywhere fails. Found at: design review (review
// 3e7c F1); regression test at: unit, the source of the repository.

// daemonListenSites are the daemon's TCP listeners E1 names, by file.
var daemonListenSites = map[string]string{
	"internal/observability/metrics.go": "metrics.addr",
	// #581 D2a moved the three binds out of Container.Run, so a start binds
	// every listener before it serves any.
	"internal/service/container_listeners.go": "server.address, hosting.operations_address, node.relay_ingress.addr",
}

// excludedListenSites are listeners of other programs, each with its reason.
var excludedListenSites = map[string]string{
	"cmd/vornik-broker/main.go": "a separate program; its port is the image build's to keep apart (vornik-cloud#21)",
}

// excludedListenTrees are source trees of programs that are never part of the
// daemon binary, each with its reason.
var excludedListenTrees = map[string]string{
	"test/": "stand-in servers and fakes the end-to-end tests start; never built into the daemon",
}

func excludedTree(rel string) bool {
	for prefix := range excludedListenTrees {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// The shapes are a closed list (review 07e9): ListenAndServe and
// ListenAndServeTLS by method name (over-counting is the safe side), and
// net.Listen / net.ListenTCP. A listener opened another way (a
// net.ListenConfig, a third-party server) is not seen; a new shape joins the
// list here in the change that introduces it.
//
// listenCalls returns the TCP listen call sites in one Go file. The net
// package is found through the file's imports, whatever its local name, and
// a Listen whose network is not a string constant is counted, so neither an
// alias nor a variable network hides a listener (review bcf3 F2).
func listenCalls(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	netNames := map[string]bool{}
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "net" {
			name := "net"
			if imp.Name != nil {
				name = imp.Name.Name
			}
			netNames[name] = true
		}
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "ListenAndServe", "ListenAndServeTLS":
			out = append(out, fset.Position(call.Pos()).String())
		case "Listen", "ListenTCP":
			x, ok := sel.X.(*ast.Ident)
			if !ok || !netNames[x.Name] || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				out = append(out, fset.Position(call.Pos()).String()+" (network not a constant)")
				return true
			}
			if network, _ := strconv.Unquote(lit.Value); strings.HasPrefix(network, "tcp") {
				out = append(out, fset.Position(call.Pos()).String())
			}
		}
		return true
	})
	return out
}

func TestDaemonListeners566_AreTheFourE1Names(t *testing.T) {
	root := moduleRoot(t)
	var daemon, other []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, path, d.Name(), "testdata", "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, site := range listenCalls(t, path) {
			switch {
			case daemonListenSites[rel] != "":
				daemon = append(daemon, site)
			case excludedListenSites[rel] != "", excludedTree(rel):
			default:
				other = append(other, rel+": "+site)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(other)
	if len(daemon) != 4 {
		t.Errorf("the daemon opens %d TCP listeners, E1 names four: %v. Add the new listener's port to the image-servers rule (config hosting.go) and to E1, then update this count", len(daemon), daemon)
	}
	if len(other) != 0 {
		t.Errorf("a TCP listener outside the named files: %v. A daemon listener joins E1 and daemonListenSites; another program's joins excludedListenSites with its reason", other)
	}
}

// The counter itself: a commented-out call is not a site, and each call shape
// counts once.
func TestDaemonListeners566_CounterSeesEachShape(t *testing.T) {
	src := `package x
import ("net"; n2 "net"; "net/http")
func f(s *http.Server, network string) {
	_, _ = n2.Listen("tcp", ":3")
	_, _ = net.Listen(network, ":4")
	// s.ListenAndServe() in a comment is not a site
	_ = s.ListenAndServe()
	_ = s.ListenAndServeTLS("", "")
	_, _ = net.Listen("tcp", ":1")
	_, _ = net.Listen("tcp4", ":2")
	_, _ = net.Listen("unix", "/s")
	_, _ = net.ListenTCP("tcp", nil)
}`
	p := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listenCalls(t, p); len(got) != 7 {
		t.Fatalf("listenCalls = %d sites %v, want 7 (an aliased import and a variable network count; the unix listener and the comment do not)", len(got), got)
	}
}

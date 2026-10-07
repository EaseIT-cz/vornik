package configassist

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentloop"
)

// Test 1 (design §9): the assistant never execs. Two allowlists, neither
// able to satisfy the other by accident: agentloop's own test permits the
// fixed git/converter programs inside the agent container; THIS test asserts (a) this package
// spawns nothing and imports no os/exec, and (b) every handler agentloop
// registers with subprocess reachability is OUT of the envelope — so no
// subprocess in the callgraph is reachable through the daemon's Dispatch.
func TestEnvelope_NeverExecs(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); p == "os/exec" || strings.HasSuffix(p, "/exec") {
				t.Errorf("%s imports %s — the assistant never execs (2026-08-03 ruling, design §1)", e.Name(), p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && (pkg.Name == "exec" || pkg.Name == "syscall") && strings.HasPrefix(sel.Sel.Name, "Command") || ok && pkg.Name == "syscall" && strings.HasPrefix(sel.Sel.Name, "Exec") {
					t.Errorf("%s:%d spawns a subprocess", e.Name(), fset.Position(call.Pos()).Line)
				}
			}
			return true
		})
	}

	// (b) follow registrations through local functions, including helpers in
	// another file and exec constructors assigned to injected function fields.
	files := map[string]*ast.File{}
	entries, err = os.ReadDir(filepath.Join("..", "agentloop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join("..", "agentloop", entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = file
	}
	registered := subprocessHandlerNames(files)
	// Issue #71: the law must examine the new converter, not only git.
	foundRenderer := false
	for _, name := range registered {
		if name == "document_render" {
			foundRenderer = true
		}
	}
	if !foundRenderer {
		t.Fatal("document_render subprocess handler was not examined by the no-exec law")
	}
	t.Logf("examined %d production agentloop files; %d subprocess-bearing handlers: %v", len(files), len(registered), registered)
	for _, n := range registered {
		if Allowed(n) {
			t.Errorf("%q reaches a subprocess-bearing agentloop function and is IN the envelope", n)
		}
		if _, ok := Classify(n); !ok {
			t.Errorf("%q reaches a subprocess but is not classified — it must be an explicit OUT", n)
		}
	}
}

type processReach struct {
	process bool
	names   map[string]bool
}

// subprocessHandlerNames computes conservative package-local reachability.
// Function references count as edges (not just calls), covering an injected
// exec.CommandContext and aliases through top-level variables. Method names
// share a node conservatively; an ambiguous name may deny, never silently allow.
func subprocessHandlerNames(files map[string]*ast.File) []string {
	nodes := map[string][]processReach{}
	handlers := map[string][]processReach{}
	for _, file := range files {
		imports := map[string]string{}
		for _, imp := range file.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imports[name] = path
		}
		inspect := func(node ast.Node) processReach {
			reach := processReach{names: map[string]bool{}}
			if node == nil {
				return reach
			}
			ast.Inspect(node, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					reach.names[id.Name] = true
					if imports["."] == "os/exec" && strings.HasPrefix(id.Name, "Command") ||
						imports["."] == "os" && id.Name == "StartProcess" ||
						imports["."] == "syscall" && (id.Name == "Exec" || id.Name == "ForkExec" || id.Name == "StartProcess") {
						reach.process = true
					}
				}
				if sel, ok := n.(*ast.SelectorExpr); ok {
					reach.names[sel.Sel.Name] = true
					if pkg, ok := sel.X.(*ast.Ident); ok {
						path := imports[pkg.Name]
						if path == "os/exec" && strings.HasPrefix(sel.Sel.Name, "Command") ||
							path == "os" && sel.Sel.Name == "StartProcess" ||
							path == "syscall" && (sel.Sel.Name == "Exec" || sel.Sel.Name == "ForkExec" || sel.Sel.Name == "StartProcess") {
							reach.process = true
						}
					}
				}
				return true
			})
			return reach
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				nodes[fn.Name.Name] = append(nodes[fn.Name.Name], inspect(fn.Body))
			}
			if gen, ok := decl.(*ast.GenDecl); ok {
				for _, spec := range gen.Specs {
					if values, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range values.Names {
							nodes[name.Name] = append(nodes[name.Name], inspect(values))
						}
					}
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			assignment, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range assignment.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok {
					continue
				}
				id, ok := idx.X.(*ast.Ident)
				if !ok || id.Name != "Handlers" {
					continue
				}
				lit, ok := idx.Index.(*ast.BasicLit)
				if !ok || i >= len(assignment.Rhs) {
					continue
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				handlers[name] = append(handlers[name], inspect(assignment.Rhs[i]))
			}
			return true
		})
	}
	process := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, variants := range nodes {
			if process[name] {
				continue
			}
			for _, reach := range variants {
				hits := reach.process
				for dep := range reach.names {
					hits = hits || process[dep]
				}
				if hits {
					process[name] = true
					changed = true
					break
				}
			}
		}
	}
	var out []string
	for name, variants := range handlers {
		for _, reach := range variants {
			hits := reach.process
			for dep := range reach.names {
				hits = hits || process[dep]
			}
			if hits {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Issue #71: the safety gate must find a future converter and a registered
// handler reaching exec through another source file, rather than a git filename.
func TestEnvelope_SubprocessReachabilityScanner(t *testing.T) {
	sources := map[string]string{
		"handlers.go": `package agentloop
func init(){Handlers["cross_file"]=wrapper;Handlers["direct"]=direct;Handlers["safe"]=safe;Handlers["injected"]=injected}
func wrapper(){helper()}
func safe(){}
`,
		"helper.go": `package agentloop
import command "os/exec"
func helper(){command.Command("converter")}
func direct(){command.CommandContext(nil,"converter")}
var launch = command.CommandContext
func injected(){_ = launch}
`,
		"process.go": `package agentloop
import operating "os"
import sys "syscall"
func init(){Handlers["os_process"]=osProcess;Handlers["sys_process"]=sysProcess}
func osProcess(){operating.StartProcess("converter",nil,nil)}
func sysProcess(){sys.ForkExec("converter",nil,nil)}
`,
		"dot.go": `package agentloop
import . "os/exec"
func init(){Handlers["dot_import"]=dotImport}
func dotImport(){Command("converter")}
`,
	}
	files := map[string]*ast.File{}
	for name, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = file
	}
	got := subprocessHandlerNames(files)
	if want := "cross_file,direct,dot_import,injected,os_process,sys_process"; strings.Join(got, ",") != want {
		t.Fatalf("subprocess handlers = %v, want %s", got, want)
	}
}

// Test 12 (design §9): a tool agentloop declares but the envelope does not
// classify fails — the new-tool-widens-the-surface case. The envelope must
// be exactly seven IN and every OUT named with a reason.
func TestEnvelope_EveryAgentloopToolClassified(t *testing.T) {
	if u := Unclassified(); len(u) != 0 {
		t.Fatalf("agentloop tools not classified in the envelope (OUT by default, but LOUD): %v — add a Classification row with a reason", u)
	}
	want := []string{"current_time", "file_edit", "file_read", "file_write", "glob", "grep", "read_many_files"}
	got := AllowedNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("IN set = %v, want exactly %v (design §2.0)", got, want)
	}
	for _, c := range envelope {
		if strings.TrimSpace(c.Reason) == "" {
			t.Errorf("%q has no reason — a classification is a decision recorded as data", c.Name)
		}
		if _, ok := agentloop.Handlers[c.Name]; !ok {
			t.Errorf("%q is classified but agentloop does not implement it", c.Name)
		}
	}
}

func TestDispatch_RefusesOutAndUnknown(t *testing.T) {
	env := agentloop.Env{Workspace: t.TempDir()}
	if got := Dispatch(env, "git_status", json.RawMessage(`{}`)); !strings.HasPrefix(got, ErrToolOutOfEnvelope) || !strings.Contains(got, "git_status") {
		t.Errorf("git_status must be refused by name: %q", got)
	}
	if got := Dispatch(env, "run_shell", json.RawMessage(`{}`)); !strings.HasPrefix(got, ErrToolOutOfEnvelope) || !strings.Contains(got, "not classified") {
		t.Errorf("unclassified tool must be refused as OUT by default: %q", got)
	}
	// An IN tool reaches agentloop (and its own refusals, e.g. missing path).
	if got := Dispatch(env, "file_read", json.RawMessage(`{}`)); got != "ERROR: path is required" {
		t.Errorf("file_read must be dispatched to agentloop: %q", got)
	}
	// Test 2 at this seam: a path outside the root is refused by name.
	if got := Dispatch(env, "file_read", json.RawMessage(`{"path":"../../etc/passwd"}`)); !strings.Contains(got, "escapes") && !strings.Contains(got, "not found") {
		t.Errorf("path escape must be refused: %q", got)
	}
}

func TestTools_SevenDefinitionsWithConfigRootWording(t *testing.T) {
	tools := Tools()
	if len(tools) != 7 {
		t.Fatalf("Tools() = %d definitions, want 7", len(tools))
	}
	for _, tl := range tools {
		if !Allowed(tl.Function.Name) {
			t.Errorf("advertised tool %q is not IN", tl.Function.Name)
		}
		if strings.Contains(tl.Function.Description, "/app/workspace") || strings.Contains(string(tl.Function.Parameters), "/app/workspace") {
			t.Errorf("%q still describes the container workspace: %s", tl.Function.Name, tl.Function.Description)
		}
		if strings.Contains(tl.Function.Description, "run_shell") {
			t.Errorf("%q advertises run_shell, which does not exist here", tl.Function.Name)
		}
		if !json.Valid(tl.Function.Parameters) {
			t.Errorf("%q parameters are not valid JSON after rewrite", tl.Function.Name)
		}
	}
}

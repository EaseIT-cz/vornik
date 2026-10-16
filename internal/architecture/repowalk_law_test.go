package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// The repo-walk law (testing LLD §12, GitHub #384 and #366, 2026-10-15).
//
// Three releases were refused or broken by one class: a test that walks the
// source tree decided for itself which directories to skip. 2026.9.0 and
// 2026.10.6 shipped walks that skipped every dot-directory, their own root
// included, and publish-ce runs the exported tree's tests inside
// $SRC/.vornik-export, so they examined nothing there. Four more laws read the
// untracked CE export copies and failed gates on stale code. Each fix wrote
// its own helper. The one rule is now internal/testutil/repowalk.SkipDir, and
// this law makes it the only one: every filepath.Walk, filepath.WalkDir and
// fs.WalkDir in a _test.go must call repowalk.SkipDir with the walk's own root
// and its callback's path, or be named below with the reason it may not.

// repoWalkExempt lists the walk sites that do not use the rule, keyed
// "<file>:<function>", with how many walk calls the site holds and why. Each
// reason starts with its class: "temp:" (a directory the test made itself),
// "content:" (a shipped subtree whose dot-directories are shipped content),
// "embed:" (an embedded filesystem). An entry that matches nothing is stale
// and fails, so the table cannot outlive the walk it excuses.
var repoWalkExempt = map[string]struct {
	calls  int
	reason string
}{
	"internal/api/companion_bundle_toolnames_test.go:scanBundleMCPNames":     {1, "content: walks one contrib plugin bundle; its .claude-plugin/.codex-plugin directories are shipped content"},
	"internal/ui/ce_brand_guard_test.go:TestCEShippedFiles_NoRetiredPalette": {1, "content: walks docs/public, internal/ui, cmd and contrib for shipped text; contrib's dot-directories ship"},
	"test/e2e/hermes/stack_test.go:writeConfigs":                             {1, "content: copies configs/agent-templates verbatim, as make install-config-assets installs it"},
	"internal/ui/csrf_client_test.go:readTemplates":                          {1, "embed: walks the embedded templates filesystem, not the disk"},

	"internal/architecture/test_env_law_test.go:TestTestEnvLaw_SentinelDataDirUntouchedByServiceTests":               {1, "temp: lists what a run left in its own sentinel data directory"},
	"internal/cli/agent_connect_generic_test.go:TestAgentConnectGeneric_PrintsEntryAndWritesNoFile":                  {1, "temp: lists every file the command wrote under the test's HOME"},
	"internal/cli/agent_connect_test.go:TestAgentConnect_RoundTrip":                                                  {1, "temp: searches the test's HOME for the key"},
	"internal/cli/agent_connect_test.go:TestAgentConnect_APIKeyStdinReadsKeyAndNotEnv":                               {1, "temp: searches the test's HOME for the admin key"},
	"internal/cli/package_upgrade_test.go:readTree":                                                                  {1, "temp: reads back a configs tree the test wrote"},
	"internal/executor/harvest_race_359_test.go:storedOutputs.bodies":                                                {1, "temp: reads the test's own artifact store"},
	"internal/executor/staging_callsite_351_test.go:TestExecuteWorkflowAttempt_TaskInputPathsAreConfinedToNamedRows": {1, "temp: lists the files staged into the step's workspace"},
	"internal/projectdeps/install/install_test.go:TestMaterialise_MarkerPermissionsAndIdempotence":                   {1, "temp: checks the modes of every file the install wrote, dot-files included"},
	"internal/projectdoctor/secret_agent_refusal_test.go:TestAgentCredential_NeverReachesTheFlatEnvironment":         {1, "temp: searches the test's secrets directory for the canary"},
	"internal/service/agentadmin_credentials_test.go:TestAgentAdmin_CredentialSlotEndToEnd":                          {1, "temp: searches the test's config directory for the canary"},
	"internal/service/agentadmin_oauth_test.go:TestAgentAdmin_OAuthServerConnectedFromThePhone":                      {1, "temp: searches the test's config directory for the token canaries"},
	"test/e2e/hermes/accounts_lane_test.go:accountsWithoutScopedKey":                                                 {1, "temp: copies a lane HOME (its dot-directories are the harness configs) without the key file"},
	"test/e2e/hermes/agentadmin_lane_test.go:withoutKeys":                                                            {1, "temp: copies a lane HOME (its dot-directories are the harness configs) without key files"},
	"test/e2e/hermes/agentadmin_lane_test.go:dirContains":                                                            {1, "temp: searches a lane directory, dot-directories included, for a needle"},
	"test/e2e/hermes/recipes_lane_test.go:assertLoopbackOnly":                                                        {1, "temp: reads the lane's own generated project configs"},
}

// walkSite is one walk call found in a test file.
type walkSite struct {
	key       string // <file>:<function>
	pos       string // file:line
	compliant bool
}

// scanRepoWalks returns every filepath.Walk, filepath.WalkDir and fs.WalkDir
// call in one test file's source, and whether each complies (see
// callsSkipDirWithRoot).
func scanRepoWalks(rel string, src []byte) ([]walkSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	var names walkNames
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch {
		case p == "path/filepath":
			names.filepath = orDefault(name, "filepath")
		case p == "io/fs":
			names.fs = orDefault(name, "fs")
		case strings.HasSuffix(p, "/internal/testutil/repowalk"):
			names.repowalk = orDefault(name, "repowalk")
		}
	}
	isPkgCall := func(call *ast.CallExpr, pkg string, names ...string) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || pkg == "" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != pkg {
			return false
		}
		for _, n := range names {
			if sel.Sel.Name == n {
				return true
			}
		}
		return false
	}
	var sites []walkSite
	visit := func(funcName string, body ast.Node) {
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var root ast.Expr
			switch {
			case isPkgCall(call, names.filepath, "Walk", "WalkDir") && len(call.Args) == 2:
				root = call.Args[0]
			case isPkgCall(call, names.fs, "WalkDir") && len(call.Args) == 3:
				root = call.Args[1]
			default:
				return true
			}
			sites = append(sites, walkSite{
				key:       rel + ":" + funcName,
				pos:       rel + ":" + strconv.Itoa(fset.Position(call.Pos()).Line),
				compliant: callsSkipDirWithRoot(call.Args[len(call.Args)-1], root, names, isPkgCall),
			})
			return true
		})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				visit(funcDeclName(d), d.Body)
			}
		case *ast.GenDecl:
			visit("<package>", d)
		}
	}
	return sites, nil
}

// callsSkipDirWithRoot reports whether the walk's callback acts on the rule:
// it is a function literal holding an `if` whose condition calls
// repowalk.SkipDir(<the walk's root>, <the callback's path parameter>, ...)
// (alone, or as an operand of && or ||, never negated) and whose body returns
// filepath.SkipDir or fs.SkipDir. Calling the rule and ignoring its answer
// does not comply (review 20261015-07bb, finding 1).
func callsSkipDirWithRoot(cb, root ast.Expr, names walkNames, isPkgCall func(*ast.CallExpr, string, ...string) bool) bool {
	lit, ok := cb.(*ast.FuncLit)
	if !ok || len(lit.Type.Params.List) == 0 || len(lit.Type.Params.List[0].Names) == 0 {
		return false
	}
	pathParam := lit.Type.Params.List[0].Names[0].Name
	want := types.ExprString(root)
	var condCalls func(e ast.Expr) bool
	condCalls = func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.ParenExpr:
			return condCalls(e.X)
		case *ast.BinaryExpr:
			if e.Op == token.LAND || e.Op == token.LOR {
				return condCalls(e.X) || condCalls(e.Y)
			}
		case *ast.CallExpr:
			if isPkgCall(e, names.repowalk, "SkipDir") && len(e.Args) >= 3 && types.ExprString(e.Args[0]) == want {
				p, ok := e.Args[1].(*ast.Ident)
				return ok && p.Name == pathParam
			}
		}
		return false
	}
	returnsSkipDir := func(body *ast.BlockStmt) bool {
		for _, st := range body.List {
			ret, ok := st.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			sel, ok := ret.Results[0].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SkipDir" {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name != "" && (id.Name == names.filepath || id.Name == names.fs) {
				return true
			}
		}
		return false
	}
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if ifs, ok := n.(*ast.IfStmt); ok && !found && condCalls(ifs.Cond) && returnsSkipDir(ifs.Body) {
			found = true
		}
		return !found
	})
	return found
}

// walkNames are the names a file imports the walk packages under.
type walkNames struct{ filepath, fs, repowalk string }

func funcDeclName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	t := d.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	return types.ExprString(t) + "." + d.Name.Name
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// repoWalkVerdict compares what the scan found with the exemption table and
// returns the faults. present reports whether a file exists in this tree
// (an entry for a file the edition prunes is not stale there).
func repoWalkVerdict(sites []walkSite, exempt map[string]struct {
	calls  int
	reason string
}, present func(rel string) bool, enterprise bool) []string {
	bad := map[string][]string{}
	for _, s := range sites {
		if !s.compliant {
			bad[s.key] = append(bad[s.key], s.pos)
		}
	}
	var faults []string
	for key, at := range bad {
		e, ok := exempt[key]
		switch {
		case !ok:
			faults = append(faults, "walk at "+strings.Join(at, ", ")+" does not call repowalk.SkipDir(<the walk's root>, <its path parameter>, name, extra...) and "+key+" is not exempt; use the rule (testing LLD §12), or exempt the site with its reason")
		case e.calls != len(at):
			faults = append(faults, key+" is exempt for "+strconv.Itoa(e.calls)+" walk call(s) but holds "+strconv.Itoa(len(at))+" ("+strings.Join(at, ", ")+"): a new walk there is not covered by the old reason")
		}
	}
	for key, e := range exempt {
		if class, why, _ := strings.Cut(e.reason, ":"); (class != "temp" && class != "content" && class != "embed") || strings.TrimSpace(why) == "" {
			faults = append(faults, "exemption "+key+" has no reason of a known class (temp:, content:, embed:): "+e.reason)
		}
		if _, ok := bad[key]; ok {
			continue
		}
		file := key[:strings.LastIndex(key, ":")]
		if !present(file) && !enterprise {
			continue // pruned from this edition
		}
		faults = append(faults, "exemption "+key+" matched no walk: stale, remove it")
	}
	sort.Strings(faults)
	return faults
}

func TestRepoWalkLaw_EveryWalkUsesTheOneRule(t *testing.T) {
	root := moduleRoot(t)
	var sites []walkSite
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, path, d.Name(), "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, path)
		found, perr := scanRepoWalks(filepath.ToSlash(rel), src)
		if perr != nil {
			return perr
		}
		sites = append(sites, found...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	compliant, exemptCalls := 0, 0
	inFiles := map[string]bool{}
	byClass := map[string]int{}
	for _, s := range sites {
		inFiles[s.key[:strings.LastIndex(s.key, ":")]] = true
		if s.compliant {
			compliant++
			continue
		}
		if e, ok := repoWalkExempt[s.key]; ok {
			exemptCalls++
			class, _, _ := strings.Cut(e.reason, ":")
			byClass[class]++
		}
	}
	t.Logf("repo-walk law: examined %d _test.go files; %d walk calls in %d files: %d call repowalk.SkipDir with the walk's root, %d exempt (temp %d, content %d, embed %d) at %d named sites",
		files, len(sites), len(inFiles), compliant, exemptCalls, byClass["temp"], byClass["content"], byClass["embed"], len(repoWalkExempt))
	// Floors: a walk that broke must not pass by examining nothing.
	// 3,604 test files and 55 walk calls in the Enterprise tree on 2026-10-15;
	// the Community export prunes some of each.
	if files < 1000 || len(sites) < 30 {
		t.Fatalf("repo-walk law examined %d test files and %d walk calls (floors 1000 and 30); a law that looked at nothing has not passed", files, len(sites))
	}
	present := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	for _, f := range repoWalkVerdict(sites, repoWalkExempt, present, enterpriseTree(root)) {
		t.Error(f)
	}
}

// The law's own regression fixtures. Release 2026.10.6 (publish-ce run
// 38645387492): the walkers e587beafc fixed skipped every dot-directory, their
// own root included; that exact shape must be flagged.
func TestRepoWalkLaw_FlagsTheShapesThatShipped(t *testing.T) {
	const header = "package x\n\nimport (\n\t\"io/fs\"\n\t\"path/filepath\"\n\t\"strings\"\n\n\t\"vornik.io/vornik/internal/testutil/repowalk\"\n)\n\nvar _ = strings.HasPrefix\nvar _ = repowalk.SkipDir\n\n"
	cases := []struct {
		name      string
		body      string
		compliant bool
	}{
		{"pre-e587beafc: skips every dot-directory, its root included", `func law(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return nil
	})
}`, false},
		{"an inline rule with a root guard is still a second rule", `func law(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() && path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return nil
	})
}`, false},
		{"SkipDir named with another root than the walk's", `func law(root string) {
	base := filepath.Join(root, "internal")
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() && repowalk.SkipDir(root, path, d.Name()) {
			return filepath.SkipDir
		}
		return nil
	})
}`, false},
		{"SkipDir given another path than the callback's", `func law(root, other string) {
	_ = filepath.Walk(root, func(p string, info fs.FileInfo, err error) error {
		if info.IsDir() && repowalk.SkipDir(root, other, info.Name()) {
			return filepath.SkipDir
		}
		return nil
	})
}`, false},
		{"SkipDir called and its answer ignored", `func law(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		_ = repowalk.SkipDir(root, path, d.Name())
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return nil
	})
}`, false},
		{"SkipDir negated", `func law(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() && !repowalk.SkipDir(root, path, d.Name()) {
			return filepath.SkipDir
		}
		return nil
	})
}`, false},
		{"SkipDir whose branch does not skip", `func law(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() && repowalk.SkipDir(root, path, d.Name()) {
			return nil
		}
		return nil
	})
}`, false},
		{"the rule, with the walk's root and path", `func law(root string) {
	base := filepath.Join(root, "internal")
	_ = filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() && repowalk.SkipDir(filepath.Join(root, "internal"), path, d.Name(), "testdata") {
			return filepath.SkipDir
		}
		return nil
	})
	_ = base
}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sites, err := scanRepoWalks("x/x_test.go", []byte(header+c.body+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if len(sites) != 1 {
				t.Fatalf("found %d walk calls, want 1", len(sites))
			}
			if sites[0].compliant != c.compliant {
				t.Fatalf("compliant = %v, want %v", sites[0].compliant, c.compliant)
			}
			faults := repoWalkVerdict(sites, nil, func(string) bool { return true }, true)
			if c.compliant != (len(faults) == 0) {
				t.Fatalf("verdict faults = %v", faults)
			}
		})
	}
}

func TestRepoWalkLaw_ExemptionsAreCountedAndGoStale(t *testing.T) {
	type ex = map[string]struct {
		calls  int
		reason string
	}
	site := walkSite{key: "a/a_test.go:f", pos: "a/a_test.go:3"}
	present := func(string) bool { return true }
	if f := repoWalkVerdict([]walkSite{site}, ex{"a/a_test.go:f": {1, "temp: t.TempDir"}}, present, true); len(f) != 0 {
		t.Fatalf("an exempt site with the right count faulted: %v", f)
	}
	if f := repoWalkVerdict([]walkSite{site, site}, ex{"a/a_test.go:f": {1, "temp: t.TempDir"}}, present, true); len(f) != 1 {
		t.Fatalf("a second walk at an exempt site passed: %v", f)
	}
	if f := repoWalkVerdict(nil, ex{"a/a_test.go:f": {1, "temp: t.TempDir"}}, present, true); len(f) != 1 || !strings.Contains(f[0], "stale") {
		t.Fatalf("a stale exemption passed: %v", f)
	}
	absent := func(string) bool { return false }
	if f := repoWalkVerdict(nil, ex{"a/a_test.go:f": {1, "temp: t.TempDir"}}, absent, false); len(f) != 0 {
		t.Fatalf("an exemption for a file the Community export prunes faulted there: %v", f)
	}
	if f := repoWalkVerdict(nil, ex{"a/a_test.go:f": {1, "temp: t.TempDir"}}, absent, true); len(f) != 1 {
		t.Fatalf("an exemption for a missing file passed in the Enterprise tree: %v", f)
	}
	for _, reason := range []string{"", "because", "temp:", "other: a reason"} {
		if f := repoWalkVerdict([]walkSite{site}, ex{"a/a_test.go:f": {1, reason}}, present, true); len(f) != 1 {
			t.Fatalf("an exemption with reason %q passed: %v", reason, f)
		}
	}
}

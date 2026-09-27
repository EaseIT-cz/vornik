package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The second process-spawn law (design §3, round-1 F1; "S1b-2, as built"): the
// os/exec law pins PROGRAMS to internal/spawn, but a kind such as GitWorkspace
// could still be called from a new request handler. So the caller column is
// mechanical: only the packages below may import internal/spawn, and each may
// use only the spawn identifiers its entry lists. A new importer, or a new
// identifier in an existing one (say, internal/mcp starting to mint its own
// ConfiguredCommand), fails here and must be added in review with its reason.
//
// Scope: every non-test Go file in the module (go list ./...), not only the
// daemon graph — a package that is not linked today can be tomorrow. Test
// files are exempt: they mint commands and register roots for fixtures.
//
// Like the os/exec law it is module-agnostic and ships in the Community
// export: an entry whose package is absent is skipped.

// spawnNeutral are spawn identifiers that start nothing and mint nothing —
// types, sentinel errors, the zero ConfiguredCommand — so any allowed importer
// may name them. Everything else (a kind, the mint, the root registry) must be
// listed per package.
var spawnNeutral = map[string]bool{
	"Cmd": true, "ErrRefused": true, "ErrNotFound": true, "ExitError": true,
	"ConfiguredCommand": true, "ConfiguredOptions": true, "Kind": true,
	// S6: value types that start and register nothing.
	"GitDirs": true, "GitConfigComposition": true, "GitConfigEntry": true, "DroppedGitConfigKey": true,
}

// spawnImporters maps an allowed importer (module-relative) to the spawn
// identifiers it may use beyond spawnNeutral, and why.
var spawnImporters = map[string]struct {
	uses   []string
	reason string
}{
	"internal/runtime":                 {[]string{"PodmanAgent", "PodmanControl", "LookPodman"}, "agent containers (PodmanAgent) and their lifecycle (PodmanControl)"},
	"internal/executor":                {[]string{"GitWorkspace", "GitWorkspaceDirs", "PodmanControl"}, "task worktrees (GitWorkspace; GitWorkspaceDirs names a worktree's git dir, S6-D3); the in-use check's podman ps"},
	"internal/executor/handlers/forge": {[]string{"GitWorkspace"}, "change-request commit count, patch and HEAD"},
	"internal/forge/github":            {[]string{"GitWorkspace"}, "the change-request push"},
	"internal/autonomy":                {[]string{"GitWorkspace"}, "the backlog workspace refresh"},
	"internal/api":                     {[]string{"GitHTTPBackend"}, "git HTTP only (design §3 precondition)"},
	"internal/sandboxtool":             {[]string{"PodmanAgent", "PodmanControl"}, "the sandbox one-shot runner (S5a)"},
	"internal/mcp":                     {[]string{"ConfiguredProgram"}, "stdio MCP servers: RUNS a ConfiguredCommand, never mints one"},
	"internal/chat":                    {[]string{"ConfiguredProgram"}, "claude/codex CLI providers: RUNS a ConfiguredCommand, never mints one"},
	"internal/service":                 {[]string{"NewConfiguredCommand", "RegisterWorkspaceRoot", "ComposeGitConfig"}, "the config loader's hand-off: mints ConfiguredCommands from loaded config, registers the workspace root, and at startup composes daemon git's global config (S6-D4, which reads the operator's config inside spawn); nothing request-reachable"},
	"cmd/mcp-bridge":                   {[]string{"NewConfiguredCommand"}, "in-sandbox bridge: mints from its own mcp.json"},
}

const spawnImportPath = "internal/spawn"

// spawnUse is what one package does with internal/spawn: the identifiers it
// selects from it, across its non-test files.
type spawnUse map[string]bool

// spawnImporterViolations is the law's judgement, separated from go list and
// the parser so a planted violator can prove it fails (design §6, self-test).
// importers maps a module-relative package to its use of spawn; present lists
// every module-relative package in the module.
func spawnImporterViolations(importers map[string]spawnUse, present map[string]bool) []string {
	var out []string
	for pkg, use := range importers {
		entry, ok := spawnImporters[pkg]
		if !ok {
			out = append(out, pkg+" imports internal/spawn but is not an allowed importer")
			continue
		}
		allowed := map[string]bool{}
		for _, u := range entry.uses {
			allowed[u] = true
		}
		for id := range use {
			if !allowed[id] && !spawnNeutral[id] {
				out = append(out, pkg+" uses spawn."+id+", which its spawnImporters entry does not list")
			}
		}
	}
	for pkg := range spawnImporters {
		if present[pkg] {
			if _, ok := importers[pkg]; !ok {
				out = append(out, pkg+" no longer imports internal/spawn: remove it from spawnImporters")
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestSpawnLaw_ImporterViolationsSelfTest(t *testing.T) {
	present := map[string]bool{"internal/ui": true, "internal/mcp": true, "internal/api": true}
	got := spawnImporterViolations(map[string]spawnUse{
		"internal/ui":  {"GitWorkspace": true},                                                 // a request handler
		"internal/mcp": {"ConfiguredProgram": true, "Cmd": true, "NewConfiguredCommand": true}, // minting where it may only run
	}, present)
	want := []string{
		"internal/api no longer imports internal/spawn: remove it from spawnImporters",
		"internal/mcp uses spawn.NewConfiguredCommand, which its spawnImporters entry does not list",
		"internal/ui imports internal/spawn but is not an allowed importer",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("self-test:\n got %q\nwant %q", got, want)
	}
}

func TestSpawnLaw_OnlyNamedPackagesImportSpawn(t *testing.T) {
	root, module := moduleRootAndPath(t)
	spawnPath := module + "/" + spawnImportPath

	present := map[string]bool{}
	importers := map[string]spawnUse{}
	examined := 0
	for _, line := range depsGoList(t, root, "-f", `{{.ImportPath}}|{{.Dir}}|{{join .GoFiles " "}}|{{join .Imports " "}}`, "./...") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		rel, ok := strings.CutPrefix(parts[0], module+"/")
		if !ok {
			continue
		}
		present[rel] = true
		examined++
		if rel == spawnImportPath || !containsField(parts[3], spawnPath) {
			continue
		}
		use := spawnUse{}
		for _, f := range strings.Fields(parts[2]) {
			for id := range spawnSelectors(t, filepath.Join(parts[1], f), spawnPath) {
				use[id] = true
			}
		}
		importers[rel] = use
	}
	if !present[spawnImportPath] {
		t.Skip("internal/spawn is absent from this edition")
	}
	t.Logf("examined %d packages; %d import internal/spawn", examined, len(importers))
	for _, v := range spawnImporterViolations(importers, present) {
		t.Error(v)
	}
}

// spawnSelectors returns the identifiers a file selects from the spawn
// package, or nothing when it does not import it.
func spawnSelectors(t *testing.T, file, spawnPath string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	name := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == spawnPath {
			name = "spawn"
			if imp.Name != nil {
				name = imp.Name.Name
			}
		}
	}
	out := map[string]bool{}
	if name == "" {
		return out
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
				out[sel.Sel.Name] = true
			}
		}
		return true
	})
	return out
}

// The git http-backend precondition (design §3, round-1 F3): allowlisting it is
// sound only while a project repository's hooks/ is written by the daemon's
// guard installer alone. This scans every daemon-linked non-test file for a
// string literal that names a hooks path — "hooks" as a path segment, or
// hooksPath in any case — and fails outside executor.EnsureReceiveGuards. It
// fails too when the installer itself no longer names it, so it cannot pass by
// finding nothing.
//
// What it does NOT see: a hooks path that arrives as data (a proposal's op
// path, a configured file path) or is assembled from pieces. Those writers are
// pinned behaviourally where they were found (the apply engine's and the
// backlog path's refusal of any .git segment; see the design's "S1b-2, as
// built").
func TestSpawnLaw_OnlyTheGuardInstallerNamesHooks(t *testing.T) {
	root, module := moduleRootAndPath(t)

	files := map[string]bool{}
	for _, main := range spawnLawDaemonMains {
		if !depsGoListOK(root, "./"+main) {
			continue
		}
		for _, line := range depsGoList(t, root, "-deps", "-f", `{{.ImportPath}}|{{.Dir}}|{{join .GoFiles " "}}`, "./"+main) {
			parts := strings.SplitN(line, "|", 3)
			if len(parts) != 3 || !strings.HasPrefix(parts[0], module+"/") {
				continue
			}
			for _, f := range strings.Fields(parts[2]) {
				files[filepath.Join(parts[1], f)] = true
			}
		}
	}
	if len(files) == 0 {
		t.Fatal("no daemon-linked files found: the scan examined nothing")
	}

	const installerFile, installerFunc = "internal/executor/worktree.go", "EnsureReceiveGuards"
	installerHits := 0
	// Sites that name a hooks path to REFUSE it, never to write one. Each must
	// still name it (a hit count of zero fails), so an entry cannot outlive
	// its reason.
	refusers := map[[2]string]string{
		{"internal/spawn/gitconfig.go", "gitKeyVerdict"}: "S6-D4 drops core.hooksPath from daemon git's composed global config",
	}
	refuserHits := map[[2]string]int{}
	for file := range files {
		rel, _ := filepath.Rel(root, file)
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil || !namesHooksPath(v) {
					return true
				}
				if rel == installerFile && fn != nil && fn.Name.Name == installerFunc {
					installerHits++
					return true
				}
				if fn != nil {
					if _, ok := refusers[[2]string{rel, fn.Name.Name}]; ok {
						refuserHits[[2]string{rel, fn.Name.Name}]++
						return true
					}
				}
				t.Errorf("%s: %q names a git hooks path outside %s.%s: only the guard installer may write a project's hooks/ (design §3, git http-backend precondition)",
					fset.Position(lit.Pos()), v, installerFile, installerFunc)
				return true
			})
		}
	}
	if installerHits == 0 {
		t.Errorf("%s.%s no longer names the hooks directory: the law is examining nothing", installerFile, installerFunc)
	}
	for site, why := range refusers {
		if _, linked := files[filepath.Join(root, filepath.FromSlash(site[0]))]; linked && refuserHits[site] == 0 {
			t.Errorf("%s.%s is allowed to name a hooks path (%s) but no longer does: remove it from the refusers", site[0], site[1], why)
		}
	}
	t.Logf("examined %d daemon-linked files; the installer names hooks %d time(s)", len(files), installerHits)
}

// namesHooksPath reports whether a string literal names a git hooks path:
// "hooks" as a whole path segment, or hooksPath in any case. A host name such as
// hooks.slack.com is not a path segment.
func namesHooksPath(v string) bool {
	if strings.Contains(strings.ToLower(v), "hookspath") {
		return true
	}
	for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == "hooks" {
			return true
		}
	}
	return false
}

func TestSpawnLaw_NamesHooksPath(t *testing.T) {
	for v, want := range map[string]bool{
		"hooks": true, ".git/hooks": true, "hooks/pre-receive": true, "core.hooksPath": true,
		"hooks.slack.com": false, "webhooks": false, "/api/v1/webhooks/": false, "": false,
	} {
		if got := namesHooksPath(v); got != want {
			t.Errorf("namesHooksPath(%q) = %v, want %v", v, got, want)
		}
	}
}

func moduleRootAndPath(t *testing.T) (string, string) {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(out)))
	return root, depsGoList(t, root, "-m")[0]
}

func containsField(s, want string) bool {
	for _, f := range strings.Fields(s) {
		if f == want {
			return true
		}
	}
	return false
}

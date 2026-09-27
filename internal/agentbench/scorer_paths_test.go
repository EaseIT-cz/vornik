package agentbench

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The staleness guard's scorer-path list cannot silently go stale (agent
// benchmark design §12.20.3, review 9c51 F4). The guard refuses a pinned
// harness when a SCORER path changed after it was built; a scorer that moved
// into a package missing from that list would pass the guard for exactly the
// change it exists to catch. So every repo-local package the scorer imports
// must be in the list or excluded here with a reason.

const repoModule = "vornik.io/vornik/"

// scorerImportExclusions: imported by the scorer, deliberately not scorer
// paths. Each reason is why a change there cannot change a journal's numbers.
var scorerImportExclusions = map[string]string{
	"internal/chat":        "wire types for requests/usage; routing and providers are not scoring",
	"internal/persistence": "the ledger schema; the harness's reads of it are internal/agentbench/store_sql.go, which is listed",
	"internal/version":     "build stamping only",
	"internal/swarmclass":  "membench's scope guard refusing trading swarms — decides whether a run may start, not what it scores",
}

func guardScorerPaths(t *testing.T) []string {
	t.Helper()
	// Relative paths here and below assume go test's default CWD, the
	// package directory; a runner that changes it fails loudly on this read.
	data, err := os.ReadFile("../../scripts/bench-harness-staleness-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^BENCH_SCORER_PATHS=\(([^)]*)\)`).FindSubmatch(data)
	if m == nil {
		t.Fatal("BENCH_SCORER_PATHS not found in the guard script")
	}
	var out []string
	for _, f := range strings.Fields(string(m[1])) {
		out = append(out, strings.Trim(f, `'"`))
	}
	return out
}

func coveredByGuard(pkg string, paths []string) bool {
	for _, p := range paths {
		if ok, _ := filepath.Match(p, pkg); ok || p == pkg || strings.HasPrefix(pkg, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

// repoImports returns the repo-local packages imported by the non-test Go
// files matching glob.
func repoImports(t *testing.T, glob string) []string {
	t.Helper()
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		t.Fatalf("no files for %s: %v", glob, err)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, repoModule) {
				seen[strings.TrimPrefix(p, repoModule)] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TestScorerPaths_EveryScorerImportIsListedOrExcluded(t *testing.T) {
	paths := guardScorerPaths(t)
	// Not vacuous (review f2ec N2): if repoModule drifted from go.mod, every
	// import would read as external and the test would pass having examined
	// nothing. The scorer's own sibling packages must be seen.
	if got := repoImports(t, "*.go"); !contains(got, "internal/quality") || !contains(got, "internal/membench") {
		t.Fatalf("repo-local imports of internal/agentbench = %v; repoModule %q no longer matches go.mod?", got, repoModule)
	}
	// internal/cli is FILE-scoped in the guard (bench_agent*.go), not a package
	// (review f2ec N3): the rest of that package is not scorer code. So no
	// scorer file may import internal/cli — that would put non-bench CLI code
	// in the scoring path without the guard seeing it.
	for _, src := range []string{"*.go", "../quality/*.go", "../membench/*.go"} {
		if contains(repoImports(t, src), "internal/cli") {
			t.Errorf("%s imports internal/cli, which the guard covers only as bench_agent*.go files", src)
		}
	}
	for _, want := range []string{"internal/agentbench", "internal/quality", "internal/membench"} {
		if !coveredByGuard(want, paths) {
			t.Errorf("the guard does not list %s", want)
		}
	}
	for _, src := range []string{"*.go", "../quality/*.go", "../membench/*.go", "../cli/bench_agent*.go"} {
		for _, pkg := range repoImports(t, src) {
			if coveredByGuard(pkg, paths) {
				continue
			}
			if _, ok := scorerImportExclusions[pkg]; ok {
				continue
			}
			t.Errorf("%s (imported by %s) is neither a scorer path in bench-harness-staleness-guard.sh "+
				"nor an exclusion with a reason here — a scorer change there would pass the guard", pkg, src)
		}
	}
}

package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// GitHub #749 item 2 (2026-11-13 batch review of #662; live-task-observation
// design, amendment 2026-11-12). The Postgres ExecutionLiveEventRepository
// holds a striped in-process mutex across its INSERT. That is safe only while
// the repository is built over the connection pool: built over a transaction,
// a second instance has its own stripes and a caller's transaction would hold
// the row locks the stripe was meant to order. It was a comment, checked by
// hand. This law makes it a census: every non-test site that builds the
// repositories (storage.Build) or the live-event repository directly names the
// argument it passes, and each (file, argument) pair is a reviewed entry.
var liveEventPoolSites = map[string]string{
	"internal/service/container.go|c.instrumentedDB()":            "the daemon's pool, wrapped with metrics",
	"internal/service/container_subsystems.go|c.instrumentedDB()": "the daemon's pool, wrapped with metrics",
	"internal/storage/storage.go|pgDB.DB":                         "storage.Open's *sql.DB pool",
	"internal/storage/storage.go|dbtx":                            "inside Build, whose callers are the entries above",
}

// storagePkgSuffix is declared with the SQLite fixture law.
const postgresPkgSuffix = "/internal/persistence/postgres"

// liveEventPoolSitesIn returns the (file|argument) keys of one non-test file's
// storage.Build and NewExecutionLiveEventRepository calls. rel is
// module-relative with forward slashes.
func liveEventPoolSitesIn(rel string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	pkgName := map[string]string{} // import name -> path suffix it reaches
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		for _, suffix := range []string{storagePkgSuffix, postgresPkgSuffix} {
			if strings.HasSuffix(p, suffix) {
				name := filepath.Base(p)
				if imp.Name != nil {
					name = imp.Name.Name
				}
				pkgName[name] = suffix
			}
		}
	}
	var sites []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		var fn, suffix string
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fun.X.(*ast.Ident); ok {
				fn, suffix = fun.Sel.Name, pkgName[x.Name]
			}
		case *ast.Ident: // a call from inside the package itself
			fn = fun.Name
			switch dir {
			case "internal/storage":
				suffix = storagePkgSuffix
			case "internal/persistence/postgres":
				suffix = postgresPkgSuffix
			}
		}
		if (suffix == storagePkgSuffix && fn == "Build") ||
			(suffix == postgresPkgSuffix && fn == "NewExecutionLiveEventRepository") {
			sites = append(sites, rel+"|"+types.ExprString(call.Args[0]))
		}
		return true
	})
	return sites, nil
}

// liveEventPoolLawCheck walks root's non-test Go files and compares the sites
// against liveEventPoolSites.
func liveEventPoolLawCheck(tb testing.TB, root string, floor int) {
	tb.Helper()
	got := map[string]bool{}
	examined := 0
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
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		examined++
		sites, err := liveEventPoolSitesIn(filepath.ToSlash(rel), raw)
		if err != nil {
			return err
		}
		for _, s := range sites {
			got[s] = true
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	requireExamined(tb, "live-event pool law", "non-test Go files", examined, floor)
	var unreviewed, stale []string
	for s := range got {
		if _, ok := liveEventPoolSites[s]; !ok {
			unreviewed = append(unreviewed, s)
		}
	}
	for s := range liveEventPoolSites {
		if !got[s] {
			stale = append(stale, s)
		}
	}
	sort.Strings(unreviewed)
	sort.Strings(stale)
	if len(unreviewed) > 0 {
		tb.Errorf("storage.Build or NewExecutionLiveEventRepository called with an unreviewed argument: %v. The live-event repository's append lock is safe only over the connection pool, never a transaction (GitHub #662, #749); pass the pool and list the site in liveEventPoolSites", unreviewed)
	}
	if len(stale) > 0 {
		tb.Errorf("liveEventPoolSites entries with no such call any more: %v", stale)
	}
}

func TestLiveEventPool749_RepositoriesAreBuiltOverThePool(t *testing.T) {
	liveEventPoolLawCheck(t, moduleRoot(t), 1000)
}

// The law must fail on the shape it exists to refuse: storage.Build over a
// transaction, and the live-event repository built outside storage.Build.
func TestLiveEventPool749_RefusesATransaction(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/jobs/tx.go", `package jobs
import (
	st "example.com/m/internal/storage"
	"example.com/m/internal/persistence/postgres"
)
func f(tx any) { _ = st.Build(tx); _ = postgres.NewExecutionLiveEventRepository(tx) }
`)
	errs := recordLaw(t, func(tb testing.TB) { liveEventPoolLawCheck(tb, root, 1) })
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"internal/jobs/tx.go|tx", "unreviewed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the law did not refuse a transaction-built repository (want %q): %q", want, errs)
		}
	}
}

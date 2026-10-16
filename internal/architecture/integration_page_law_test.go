package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// The integration page law (storage-abstraction design, "The Postgres
// integration lane's database", change 2026-10-15, #374). The Postgres lane
// runs six packages one after another on one database, and every test in a
// package shares it. A test that reads a FIXED page of a shared table — "the
// first N rows", "the oldest batch" — sees whatever earlier tests left on that
// page. Two such reads failed the release gates on 2026-10-14 (a data-subject
// ListRequests(ctx, 200, 0) and DequeueEmbedBatch(ctx, 10)), and a third the
// same day inside one run (ReclaimStuckFiring(ctx, cutoff, 10), crowded by ten
// rows the wipe suite left). Making every run start from a fresh database
// removed other RUNS' rows; it cannot remove the rows earlier tests in the
// same run leave. This law keeps the class visible.
//
// It examines every integration-tagged test file in the lane's packages, plus
// internal/persistence/repotest (the shared suites the Postgres contract tests
// run). A FIXED-PAGE READ is:
//
//   - a call whose callee, by name, is declared in the module's non-test Go
//     with the parameter at that position named limit, pageSize, batchSize or
//     max, and an integer literal there;
//   - a composite literal passed to a call, setting Limit or PageSize to an
//     integer literal;
//   - a string literal holding SELECT … LIMIT <digits>.
//
// It is SCOPED only when narrowed by an identifier: an argument (or, for a
// composite literal, a field) whose declared name ends in an identifier word
// is neither a literal nor a package-qualified name; for SQL, a WHERE. Every
// unscoped read must be listed below with a reason that holds WITHIN one run.
// "The database is fresh" is not one.
//
// What it does NOT examine, logged so the count is not read as the whole
// population: a limit passed through a variable or constant (counted as
// "seen, not judged"); a parameter named otherwise (n, count); a callee
// declared only in test code; whether a scoping argument is unique to rows
// the test created; paging outside the lane's packages. The callee match is by
// name, not type.
//
// Module-agnostic, so it ships in the Community export; there, listed entries
// whose file the export removed are skipped.

// integrationLaneDirs are the package trees `make test-integration` runs.
var integrationLaneDirs = []string{"test/integration", "internal/persistence/postgres", "internal/cli", "internal/memory"}

// integrationSharedSuiteDir holds the shared suites the Postgres contract
// tests run; its files carry no build tag.
const integrationSharedSuiteDir = "internal/persistence/repotest"

var (
	pageLimitParams = map[string]bool{"limit": true, "pageSize": true, "batchSize": true, "max": true}
	pageLimitFields = map[string]bool{"Limit": true, "PageSize": true}
	pageIdentWord   = regexp.MustCompile(`(?i)(id|ids|project|namespace|scope|subject|prefix|owner|principal|entity|tenant|key|digest|chunk|term)$`)
	pageSQLLimit    = regexp.MustCompile(`(?i)\bselect\b[\s\S]*\blimit\s+[0-9]+`)
	pageSQLWhere    = regexp.MustCompile(`(?i)\bwhere\b`)
)

// pageReason is why an unscoped fixed-page read is safe within one run.
type pageReason string

const (
	pagePagedThrough        pageReason = "paged through"
	pageOwnRowsFirst        pageReason = "own rows first"
	pageOrderingIndependent pageReason = "ordering-independent"
	pageUnbounded           pageReason = "unbounded"
	pageLimitIsSubject      pageReason = "limit is the subject"
	pageResetFirst          pageReason = "reset first"
	pageLiteralOwned        pageReason = "literal owned"
)

var pageReasons = map[pageReason]bool{
	pagePagedThrough: true, pageOwnRowsFirst: true, pageOrderingIndependent: true, pageUnbounded: true,
	pageLimitIsSubject: true, pageResetFirst: true, pageLiteralOwned: true,
}

type pageListing struct {
	reason pageReason
	why    string
}

// integrationPageListed is the census of unscoped fixed-page reads, keyed
// "<file>|<the read's source text, whitespace collapsed>". A key covers every
// identical read in its file.
var integrationPageListed = map[string]pageListing{
	"internal/memory/classify_attempted_330_integration_test.go|newBF().BackfillBatchAcrossProjects(ctx, 2)": {pageOwnRowsFirst,
		"the backfill reads oldest first and both rows are 30 years old; the restart check is an absence the rows would head the queue for"},
	"internal/memory/embed_queue_lease_integration_test.go|repo.DequeueEmbedBatch(ctx, 10)": {pagePagedThrough,
		"claimUntil claims until the seeded row comes back (aa2dc8d5d)"},
	"internal/persistence/postgres/data_subject_kg_resolve_integration_test.go|idx.MentionChunks(ctx, \"ent_m\", 100)": {pageResetFirst,
		"resetKGFixture deletes the project's entities and chunks before seeding; mentions cascade"},
	"internal/persistence/postgres/regulatory_record_integration_test.go|repo.ListRequests(ctx, 2, 0)": {pageOwnRowsFirst,
		"opened_at DESC, id DESC; the three requests are dated 100 years ahead"},
	"internal/persistence/postgres/regulatory_record_integration_test.go|repo.ListRequests(ctx, 2, 2)": {pageOwnRowsFirst,
		"opened_at DESC, id DESC; the three requests are dated 100 years ahead"},
	"internal/persistence/postgres/regulatory_record_integration_test.go|repo.List(ctx, 2, 0)": {pageOwnRowsFirst,
		"became_aware_at DESC, id DESC; the three incidents are dated 100 years ahead"},
	"internal/persistence/postgres/regulatory_record_integration_test.go|repo.List(ctx, 2, 2)": {pageOwnRowsFirst,
		"became_aware_at DESC, id DESC; the three incidents are dated 100 years ahead"},
	"internal/persistence/postgres/reminder_stuck_firing_integration_test.go|repo.ReclaimStuckFiring(ctx, cutoff, 10)": {pageOwnRowsFirst,
		"fired_at ASC and the row is backdated to 2001-01-01; backdateFiredAt is the lane's only writer of an old fired_at and uses no earlier date"},
	"internal/persistence/repotest/backend_debt_suites.go|fx.repo.FetchUnextracted(ctx, 500)": {pageOwnRowsFirst,
		"created_at ASC; fixtures dated 2026-09-05, before any row written at run time. The lane's one earlier-dated chunk writer (classify_attempted_330, 30 years) runs in internal/memory, after this package. The callee caps the limit at 500"},
	"internal/persistence/repotest/backend_debt_suites.go|repo.FetchUnextracted(ctx, 500)": {pageOwnRowsFirst,
		"created_at ASC; fixtures dated 2026-10-12, after only the 2026-09-05 fixtures of the suite above. Same invariant and cap"},
	"internal/persistence/repotest/backend_debt_suites.go|fx.repo.ListPendingTerminal(ctx, 500)": {pageOrderingIndependent,
		"asserts properties of each returned execution, never membership"},
	"internal/persistence/repotest/backend_debt_suites.go|repo.List(ctx, 500)": {pageOwnRowsFirst,
		"updated_at DESC; both profiles were just upserted"},
	"internal/persistence/repotest/backend_debt_suites.go|repo.List(ctx, 1)": {pageLimitIsSubject,
		"asserts the page holds one row"},
	"internal/persistence/repotest/broker_action_suite.go|h.repo.ListByStatus(h.ctx, \"broker-p\", persistence.BrokerActionPending, 100)": {pageOrderingIndependent,
		"asserts every returned row's status and project"},
	"internal/persistence/repotest/broker_grant_suite.go|h.grants.List(h.ctx, persistence.BrokerGrantFilter{Namespace: persistence.BrokerGrantAllAgentNamespaces, Limit: 500, Now: h.now})": {pageOwnRowsFirst,
		"created_at DESC; the grant is stamped with the suite's start time, so only grants this suite creates (14 createGrant calls) tie or follow it"},
	"internal/persistence/repotest/companion_push_suite.go|h.Outbox.DueTaskPushes(context.Background(), 1_000_000)": {pageUnbounded,
		"the callee replaces only limit <= 0; the row sorts last by created_at"},
	"internal/persistence/repotest/companion_push_suite.go|h.Outbox.DueActionPushes(context.Background(), 1_000_000)": {pageUnbounded,
		"the callee replaces only limit <= 0; the row sorts last by created_at"},
	"internal/persistence/repotest/coverage_gap_suites.go|repo.ClaimTimedOut(ctx, time.Now().UTC(), 1_000_000)": {pageUnbounded,
		"the callee replaces only limit <= 0; older timeouts sort first"},
	"internal/persistence/repotest/coverage_gap_suites.go|repo.LeaseDue(ctx, now, 1_000_000)": {pageUnbounded,
		"the callee replaces only limit <= 0; older fire_at sorts first"},
	"internal/persistence/postgres/reminder_task_transitions_integration_test.go|repo.LeaseDue(ctx, time.Now(), 1_000_000)": {pageUnbounded,
		"the callee replaces only limit <= 0; older fire_at sorts first"},
	"internal/persistence/postgres/reminder_stuck_firing_integration_test.go|repo.ReclaimStuckFiring(ctx, cutoff, 1_000_000)": {pageUnbounded,
		"absence assertions: the row would sort last by fired_at, so only an unbounded page can show it absent"},
	"internal/persistence/repotest/data_subject_suites.go|s.ListSubjects(ctx, 500)": {pageOwnRowsFirst,
		"newest first; both subjects were just created"},
	"internal/persistence/repotest/data_subject_suites.go|s.ListRequests(ctx, 1, 0)": {pageOrderingIndependent,
		"compared with the first two rows of a full read of the same table taken just before (685a1ee59)"},
	"internal/persistence/repotest/data_subject_suites.go|s.ListRequests(ctx, 1, 1)": {pageOrderingIndependent,
		"compared with the first two rows of a full read of the same table taken just before (685a1ee59)"},
	"internal/persistence/repotest/instinct_suite.go|repo.ListPendingRecoveryApplications(ctx, 1_000_000)": {pageUnbounded,
		"no lane table approaches a million rows"},
	"internal/persistence/repotest/repotest.go|repo.ListRecent(ctx, \"p1\", 10)": {pageLiteralOwned,
		"no other lane test writes a recovery event (the only RecoveryEvent literals in the lane are this suite's)"},
	"internal/persistence/repotest/repotest.go|repo.ListRecent(ctx, \"p2\", 10)": {pageLiteralOwned,
		"no other lane test writes a recovery event"},
	"internal/persistence/repotest/repotest.go|repo.ListRecent(ctx, \"no-such\", 10)": {pageLiteralOwned,
		"no test writes project no-such, so the page holds every such row"},
	"internal/persistence/repotest/repotest.go|repo.FindExpiredLeases(ctx, 1_000_000)": {pageUnbounded,
		"neither backend caps the limit; older expiries sort first"},
	"internal/persistence/repotest/repotest.go|repo.ListRecent(ctx, \"\", 100)": {pageOwnRowsFirst,
		"recorded_at DESC; the verdict was just recorded"},
	"internal/persistence/repotest/repotest.go|sweeper.SweepPendingForTerminalExecutions(h.ctx, 10*time.Minute, 1_000_000)": {pageUnbounded,
		"the sweep spans every project and neither backend caps a positive limit; the first pass's count is checked as at least one and the suite's rows one by one, so the second pass finds nothing left"},
	"internal/persistence/repotest/repotest.go|sweeper.SweepPendingForTerminalExecutions(h.ctx, 10*time.Minute, 0)": {pageLimitIsSubject,
		"asserts a zero limit sweeps nothing"},
	"internal/persistence/repotest/skill_suite.go|repo.ListAcrossProjects(ctx, []string{persistence.SkillMaturityActive}, 0)": {pageUnbounded,
		"0 adds no LIMIT (skill_repository.go: if limit > 0)"},
	"internal/persistence/repotest/skill_suite.go|repo.ListAcrossProjects(ctx, nil, 0)": {pageUnbounded,
		"0 adds no LIMIT (skill_repository.go: if limit > 0)"},
	"internal/persistence/repotest/skill_suite.go|repo.ListDrafts(ctx, 0)": {pageUnbounded,
		"0 adds no LIMIT (skill_repository.go: if limit > 0)"},
}

// pageParamIndex maps a callee name to, per argument position, the parameter
// names any declaration of that name gives it.
type pageParamIndex map[string]map[int]map[string]bool

func (x pageParamIndex) add(name string, ft *ast.FuncType) {
	if ft == nil || ft.Params == nil {
		return
	}
	i := 0
	for _, field := range ft.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if x[name] == nil {
				x[name] = map[int]map[string]bool{}
			}
			if x[name][i] == nil {
				x[name][i] = map[string]bool{}
			}
			x[name][i][n.Name] = true
			i++
		}
	}
}

func (x pageParamIndex) anyAt(name string, pos int, pred func(string) bool) bool {
	for p := range x[name][pos] {
		if pred(p) {
			return true
		}
	}
	return false
}

// indexPageDeclarations records every function and interface method declared
// in one non-test file.
func indexPageDeclarations(x pageParamIndex, rel string, src []byte) error {
	f, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			x.add(d.Name.Name, d.Type)
		case *ast.Field:
			if ft, ok := d.Type.(*ast.FuncType); ok && len(d.Names) == 1 {
				x.add(d.Names[0].Name, ft)
			}
		}
		return true
	})
	return nil
}

// pageRead is one fixed-page read found in a file.
type pageRead struct {
	key    string
	pos    string
	scoped bool
}

type pageCounts struct{ files, reads, scoped, unjudged int }

func isIntLiteral(e ast.Expr) bool {
	bl, ok := e.(*ast.BasicLit)
	return ok && bl.Kind == token.INT
}

// identifying reports whether e can narrow a read to rows a test created: not
// a literal, not nil, not a package-qualified name (a named constant every
// test shares).
func identifying(e ast.Expr, imports map[string]bool) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return false
	case *ast.Ident:
		return v.Name != "nil"
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok && imports[x.Name] {
			return false
		}
	}
	return true
}

func callName(c *ast.CallExpr) string {
	switch fn := c.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}

// limitComposite returns the composite literal in arg that sets a page field
// to an integer literal, if any.
func limitComposite(arg ast.Expr) *ast.CompositeLit {
	if u, ok := arg.(*ast.UnaryExpr); ok {
		arg = u.X
	}
	cl, ok := arg.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	for _, e := range cl.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && pageLimitFields[k.Name] && isIntLiteral(kv.Value) {
				return cl
			}
		}
	}
	return nil
}

func compositeScoped(cl *ast.CompositeLit, imports map[string]bool) bool {
	for _, e := range cl.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && pageIdentWord.MatchString(k.Name) && identifying(kv.Value, imports) {
				return true
			}
		}
	}
	return false
}

// judgePageFile finds the fixed-page reads in one examined file. rel is
// module-relative with forward slashes.
func judgePageFile(rel string, src []byte, idx pageParamIndex, n *pageCounts) ([]pageRead, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	n.files++
	imports := map[string]bool{}
	for _, im := range f.Imports {
		path := strings.Trim(im.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = true
	}
	text := func(node ast.Node) string {
		return strings.Join(strings.Fields(string(src[fset.Position(node.Pos()).Offset:fset.Position(node.End()).Offset])), " ")
	}
	var reads []pageRead
	add := func(node ast.Node, scoped bool) {
		n.reads++
		if scoped {
			n.scoped++
		}
		reads = append(reads, pageRead{key: rel + "|" + text(node), pos: fset.Position(node.Pos()).String(), scoped: scoped})
	}
	ast.Inspect(f, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING && pageSQLLimit.MatchString(v.Value) {
				add(v, pageSQLWhere.MatchString(v.Value))
			}
		case *ast.CallExpr:
			judgePageCall(v, idx, imports, n, add)
		}
		return true
	})
	return reads, nil
}

func judgePageCall(c *ast.CallExpr, idx pageParamIndex, imports map[string]bool, n *pageCounts, add func(ast.Node, bool)) {
	name := callName(c)
	fixed, scoped := false, false
	var composites []*ast.CompositeLit
	for i, a := range c.Args {
		if idx.anyAt(name, i, func(p string) bool { return pageLimitParams[p] }) {
			if isIntLiteral(a) {
				fixed = true
				continue
			}
			n.unjudged++
		}
		if cl := limitComposite(a); cl != nil {
			composites = append(composites, cl)
			continue
		}
		if idx.anyAt(name, i, pageIdentWord.MatchString) && identifying(a, imports) {
			scoped = true
		}
	}
	for _, cl := range composites {
		fixed = true
		scoped = scoped || compositeScoped(cl, imports)
	}
	if fixed {
		add(c, scoped)
	}
}

// checkPageCensus returns the law's violations: unscoped reads not listed,
// listings with no known reason, and listings whose read no longer occurs.
// fileExists reports whether a listed file is in this tree; ee is whether the
// tree is the Enterprise one (where every listed file must exist).
func checkPageCensus(reads []pageRead, listed map[string]pageListing, fileExists func(string) bool, ee bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range reads {
		seen[r.key] = true
		if r.scoped {
			continue
		}
		if _, ok := listed[r.key]; !ok {
			out = append(out, fmt.Sprintf("%s: unscoped fixed-page read %q: earlier tests in the same lane run can fill its page. "+
				"Scope it to rows the test created (an id argument), page through, use an unbounded limit, or list it in "+
				"integrationPageListed with a reason that holds within one run (#374)", r.pos, pageReadText(r.key)))
		}
	}
	for key, l := range listed {
		if !pageReasons[l.reason] || strings.TrimSpace(l.why) == "" {
			out = append(out, fmt.Sprintf("listing %q has reason %q and why %q: a listing needs a known reason and its argument", key, l.reason, l.why))
		}
		if seen[key] {
			continue
		}
		file, _, _ := strings.Cut(key, "|")
		if !fileExists(file) && !ee {
			continue // removed by the Community export
		}
		out = append(out, fmt.Sprintf("listing %q matches no read in the tree: remove it, or update its text", key))
	}
	sort.Strings(out)
	return out
}

// pageReadText is the read's source text, the part of a census key after the file.
func pageReadText(key string) string {
	_, text, _ := strings.Cut(key, "|")
	return text
}

func isIntegrationTagged(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if strings.HasPrefix(line, "//go:build") && strings.Contains(line, "integration") {
			return true
		}
	}
	return false
}

// examinedPageFile reports whether the law examines rel.
func examinedPageFile(rel string, src []byte) bool {
	if strings.HasPrefix(rel, integrationSharedSuiteDir+"/") && !strings.HasSuffix(rel, "_test.go") {
		return true
	}
	if !strings.HasSuffix(rel, "_test.go") {
		return false
	}
	for _, d := range integrationLaneDirs {
		if strings.HasPrefix(rel, d+"/") {
			return isIntegrationTagged(src)
		}
	}
	return false
}

// integrationPageLaw walks the module twice: once to index every declaration,
// then to judge the examined files against the whole index.
func integrationPageLaw(t testing.TB, root string) {
	t.Helper()
	idx := pageParamIndex{}
	examined := map[string][]byte{}
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
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(rel, "_test.go") {
			if err := indexPageDeclarations(idx, rel, src); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		}
		if examinedPageFile(rel, src) {
			examined[rel] = src
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	var reads []pageRead
	var n pageCounts
	for rel, src := range examined {
		r, err := judgePageFile(rel, src, idx, &n)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		reads = append(reads, r...)
	}
	listedN := 0
	for _, r := range reads {
		if !r.scoped {
			listedN++
		}
	}
	t.Logf("integration page law: %d fixed-page reads, %d scoped, %d unscoped (each must be listed); "+
		"%d limit arguments seen but not judged (a variable or constant)", n.reads, n.scoped, listedN, n.unjudged)
	if !requireExamined(t, "integration page law", "files", n.files, pageFloorFiles) ||
		!requireExamined(t, "integration page law", "fixed-page reads", n.reads, pageFloorReads) {
		return
	}
	ee := enterpriseTree(root)
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	for _, v := range checkPageCensus(reads, integrationPageListed, exists, ee) {
		t.Error(v)
	}
}

// The floors sit well under the 2026-10-15 census (155 files, 153 reads) and
// above zero, so a walk that lost the lane's files fails rather than passes.
const (
	pageFloorFiles = 50
	pageFloorReads = 50
)

func TestIntegrationPageLaw(t *testing.T) {
	integrationPageLaw(t, moduleRoot(t))
}

// The law's own tests: planted sources prove each rule fires, so a law that
// stopped recognising a read cannot pass by not seeing it.

const pagePlantedDecls = `package p

import (
	"context"
	"time"
)

type Opts struct {
	Limit     int
	ProjectID string
}

type Repo interface {
	ListRecent(ctx context.Context, projectID string, limit int) error
	Reclaim(ctx context.Context, olderThan time.Time, limit int) error
	Search(ctx context.Context, projectID, q string, opts Opts) error
	Page(ctx context.Context, limit, offset int) error
}
`

const pagePlantedTest = `//go:build integration

package p

import (
	"context"
	"testing"

	"example.com/persistence"
)

func TestPlanted(t *testing.T) {
	ctx, pid, n := context.Background(), "x", 3
	var r Repo
	r.ListRecent(ctx, pid, 10)
	r.ListRecent(ctx, "p1", 10)
	r.Reclaim(ctx, cutoff, 10)
	r.ListRecent(ctx, persistence.AllProjects, 10)
	r.Page(ctx, n, 0)
	r.Search(ctx, pid, "q", persistence.Opts{Limit: 5})
	r.Search(ctx, "", "q", persistence.Opts{Limit: 5})
	r.Search(ctx, "", "q", persistence.Opts{Limit: 5, ProjectID: pid})
	_ = ` + "`SELECT id FROM t ORDER BY at LIMIT 10`" + `
	_ = ` + "`SELECT id FROM t WHERE p = $1 LIMIT 1`" + `
}
`

func plantedPageReads(t *testing.T) ([]pageRead, pageCounts) {
	t.Helper()
	idx := pageParamIndex{}
	if err := indexPageDeclarations(idx, "internal/x/p.go", []byte(pagePlantedDecls)); err != nil {
		t.Fatal(err)
	}
	var n pageCounts
	reads, err := judgePageFile("internal/persistence/postgres/p_integration_test.go", []byte(pagePlantedTest), idx, &n)
	if err != nil {
		t.Fatal(err)
	}
	return reads, n
}

func TestIntegrationPageLaw_JudgesPlantedReads(t *testing.T) {
	reads, n := plantedPageReads(t)
	got := map[string]bool{}
	for _, r := range reads {
		got[pageReadText(r.key)] = r.scoped
	}
	want := map[string]bool{
		"r.ListRecent(ctx, pid, 10)":                                         true,
		`r.ListRecent(ctx, "p1", 10)`:                                        false, // a literal id is shared, not owned
		"r.Reclaim(ctx, cutoff, 10)":                                         false, // #374's within-run incident: a time does not scope
		"r.ListRecent(ctx, persistence.AllProjects, 10)":                     false, // a named constant is shared
		`r.Search(ctx, pid, "q", persistence.Opts{Limit: 5})`:                true,  // the call's id scopes its options
		`r.Search(ctx, "", "q", persistence.Opts{Limit: 5})`:                 false,
		`r.Search(ctx, "", "q", persistence.Opts{Limit: 5, ProjectID: pid})`: true,
		"`SELECT id FROM t ORDER BY at LIMIT 10`":                            false,
		"`SELECT id FROM t WHERE p = $1 LIMIT 1`":                            true,
	}
	for k, w := range want {
		s, ok := got[k]
		if !ok {
			t.Errorf("%s: not recognised as a fixed-page read", k)
			continue
		}
		if s != w {
			t.Errorf("%s: scoped = %v, want %v", k, s, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("found %d reads, want %d: %v", len(got), len(want), got)
	}
	if n.unjudged != 1 {
		t.Errorf("unjudged = %d, want 1 (r.Page(ctx, n, 0): a variable limit)", n.unjudged)
	}
	if n.files != 1 || n.reads != len(want) {
		t.Errorf("counts = %+v", n)
	}
}

func TestIntegrationPageLaw_Census(t *testing.T) {
	reads, _ := plantedPageReads(t)
	file := "internal/persistence/postgres/p_integration_test.go"
	listed := map[string]pageListing{}
	for _, r := range reads {
		if !r.scoped {
			listed[r.key] = pageListing{pageOrderingIndependent, "planted"}
		}
	}
	exists := func(string) bool { return true }
	if v := checkPageCensus(reads, listed, exists, true); len(v) != 0 {
		t.Fatalf("every unscoped read listed: want no violations, got %v", v)
	}

	unlisted := map[string]pageListing{}
	for k, l := range listed {
		unlisted[k] = l
	}
	delete(unlisted, file+"|r.Reclaim(ctx, cutoff, 10)")
	if v := checkPageCensus(reads, unlisted, exists, true); len(v) != 1 || !strings.Contains(v[0], "r.Reclaim(ctx, cutoff, 10)") {
		t.Errorf("an unlisted unscoped read must fail the law, got %v", v)
	}

	bad := map[string]pageListing{}
	for k, l := range listed {
		bad[k] = l
	}
	bad[file+"|r.Reclaim(ctx, cutoff, 10)"] = pageListing{"the database is fresh", "planted"}
	if v := checkPageCensus(reads, bad, exists, true); len(v) != 1 || !strings.Contains(v[0], "known reason") {
		t.Errorf("a listing with an unknown reason must fail the law, got %v", v)
	}

	stale := map[string]pageListing{}
	for k, l := range listed {
		stale[k] = l
	}
	stale[file+"|r.Gone(ctx, 5)"] = pageListing{pageUnbounded, "planted"}
	if v := checkPageCensus(reads, stale, exists, true); len(v) != 1 || !strings.Contains(v[0], "matches no read") {
		t.Errorf("a listing that matches nothing must fail the law, got %v", v)
	}
	stale["internal/removed/x_test.go|r.Gone(ctx, 5)"] = pageListing{pageUnbounded, "planted"}
	delete(stale, file+"|r.Gone(ctx, 5)")
	missing := func(rel string) bool { return rel == file }
	if v := checkPageCensus(reads, stale, missing, false); len(v) != 0 {
		t.Errorf("Community export: a listing whose file the export removed is skipped, got %v", v)
	}
	if v := checkPageCensus(reads, stale, missing, true); len(v) != 1 {
		t.Errorf("Enterprise tree: a listing whose file is missing is stale, got %v", v)
	}
}

func TestIntegrationPageLaw_ExaminedFiles(t *testing.T) {
	tagged := []byte("//go:build integration\n\npackage p\n")
	untagged := []byte("package p\n")
	for _, c := range []struct {
		rel  string
		src  []byte
		want bool
	}{
		{"internal/persistence/postgres/a_integration_test.go", tagged, true},
		{"test/integration/a_test.go", tagged, true},
		{"internal/memory/sub/a_test.go", tagged, true},
		{"internal/persistence/postgres/a_test.go", untagged, false},
		{"internal/persistence/repotest/suite.go", untagged, true},
		{"internal/persistence/repotest/suite_test.go", untagged, false},
		{"internal/service/a_test.go", tagged, false},
		{"internal/persistence/postgres/repo.go", tagged, false},
	} {
		if got := examinedPageFile(c.rel, c.src); got != c.want {
			t.Errorf("examinedPageFile(%s) = %v, want %v", c.rel, got, c.want)
		}
	}
}

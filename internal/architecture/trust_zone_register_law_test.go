package architecture

// Trust-zone register law (GitHub #338; reference-architecture LLD §11 and the
// public reference page, "Trust zones and invariants").
//
// The reference architecture publishes a table of trust-zone invariants, each
// naming the test that fails when it breaks, and a denominator ("N invariants
// stated, M pinned by a test"). This law keeps the table true:
//   - every row's "Pinned by" cell names at least one test as `TestName`;
//   - every named test is declared (`func TestName(`) in exactly one _test.go
//     file of the module — none means a dead reference, two means the name is
//     ambiguous and the row does not say which control it means;
//   - every row's status is one of the closed vocabulary;
//   - the published denominator matches the rows;
//   - the published HOLDS/SCOPED/GAP split matches the rows;
//   - no pin is declared only in a file the default `go test` lane does not
//     build (a build tag such as integration, another platform's suffix);
//   - when the EE design record is present, it lists the same ids, statuses
//     and pins.
//
// The public page names tests, not file paths: public docs may not cite
// internal/ paths (the docs-ip lint), so the law resolves each name itself.
//
// A renamed or deleted test therefore fails here instead of leaving the page
// claiming a control that no longer exists. audit 2026-10-12 (w3-5): the
// invariants were stated across a dozen designs and few had a test.

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

const (
	trustZonePublicPage = "docs/public/reference/reference-architecture.md"
	trustZoneLLD        = "https://docs.vornik.io"
)

var (
	tzRowRe         = regexp.MustCompile(`^\|\s*(TZ-\d+)\s*\|`)
	tzPinRe         = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
	tzDenominatorRe = regexp.MustCompile(`\*\*(\d+) invariants stated, (\d+) pinned by at least one test \((\d+) test references\)\*\*`)

	tzStatusCountRe = regexp.MustCompile(`(\d+) hold as stated,\s+(\d+) hold only for the\s+scope given in the row \(\*\*SCOPED\*\*\),\s+and\s+(\d+) have a known gap`)
	tzStatusRe      = regexp.MustCompile(`^\**([A-Za-z]+)`)
	tzStatuses      = map[string]bool{"HOLDS": true, "SCOPED": true, "GAP": true}
)

// tzGatedPrefix marks a declaring file that carries a //go:build constraint.
const tzGatedPrefix = "gated:"

type tzRow struct {
	id     string
	pins   []string
	status string
	cells  int
}

// parseTrustZoneTable extracts the TZ rows of a page: | ID | Invariant |
// Pinned by | Status |. Pins come from the third cell only, so a test named in
// the prose of another cell (an inverted or uncommitted test cited as
// evidence) is not mistaken for a pin.
func parseTrustZoneTable(page string) []tzRow {
	var rows []tzRow
	for _, line := range strings.Split(page, "\n") {
		m := tzRowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		row := tzRow{id: m[1], cells: len(cells)}
		if len(cells) >= 4 {
			for _, p := range tzPinRe.FindAllStringSubmatch(cells[2], -1) {
				row.pins = append(row.pins, p[1])
			}
			if s := tzStatusRe.FindStringSubmatch(strings.TrimSpace(cells[3])); s != nil {
				row.status = s[1]
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// checkTrustZoneRegister returns every violation of the register rules.
// declarations maps a test name to the files that declare it.
func checkTrustZoneRegister(page string, declarations map[string][]string) (rows []tzRow, problems []string) {
	rows = parseTrustZoneTable(page)
	if len(rows) == 0 {
		return nil, []string{"no TZ-n rows found: the trust-zone table is missing"}
	}
	pinned, refs := 0, 0
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.id] {
			problems = append(problems, r.id+": duplicate id")
		}
		seen[r.id] = true
		if r.cells != 4 {
			problems = append(problems, fmt.Sprintf("%s: %d cells, want 4 (ID | Invariant | Pinned by | Status)", r.id, r.cells))
			continue
		}
		if !tzStatuses[r.status] {
			problems = append(problems, fmt.Sprintf("%s: status %q is not one of HOLDS, SCOPED, GAP", r.id, r.status))
		}
		if len(r.pins) == 0 {
			problems = append(problems, r.id+": the Pinned by cell names no test (want `TestName`)")
			continue
		}
		pinned++
		refs += len(r.pins)
		for _, p := range r.pins {
			switch files := declarations[p]; len(files) {
			case 1:
				if strings.HasPrefix(files[0], tzGatedPrefix) {
					problems = append(problems, fmt.Sprintf("%s: %s is declared only in a build-constrained file (%s), "+
						"which the default test lane does not run", r.id, p, strings.TrimPrefix(files[0], tzGatedPrefix)))
				}
			case 0:
				problems = append(problems, fmt.Sprintf("%s: no _test.go file declares func %s(", r.id, p))
			default:
				problems = append(problems, fmt.Sprintf("%s: %s is declared in %d files (%s); the row is ambiguous",
					r.id, p, len(files), strings.Join(files, ", ")))
			}
		}
	}
	m := tzDenominatorRe.FindStringSubmatch(page)
	switch {
	case m == nil:
		problems = append(problems, "the page does not publish the denominator "+
			"(**N invariants stated, M pinned by at least one test (K test references)**)")
	case m[1] != fmt.Sprint(len(rows)) || m[2] != fmt.Sprint(pinned) || m[3] != fmt.Sprint(refs):
		problems = append(problems, fmt.Sprintf("published denominator says %s stated / %s pinned / %s references; the table has %d / %d / %d",
			m[1], m[2], m[3], len(rows), pinned, refs))
	}
	if c := tzStatusCountRe.FindStringSubmatch(page); c == nil {
		problems = append(problems, "the page does not publish the HOLDS/SCOPED/GAP split "+
			"(N hold as stated, M hold only for the scope given in the row (**SCOPED**), and K have a known gap)")
	} else {
		n := map[string]int{}
		for _, r := range rows {
			n[r.status]++
		}
		if c[1] != fmt.Sprint(n["HOLDS"]) || c[2] != fmt.Sprint(n["SCOPED"]) || c[3] != fmt.Sprint(n["GAP"]) {
			problems = append(problems, fmt.Sprintf("published split says %s HOLDS / %s SCOPED / %s GAP; the table has %d / %d / %d",
				c[1], c[2], c[3], n["HOLDS"], n["SCOPED"], n["GAP"]))
		}
	}
	return rows, problems
}

// defaultLaneBuilds reports whether `go test` with no -tags builds the file
// on this platform. go/build evaluates the //go:build expression (so
// `!integration` is built and `integration` is not) and the _GOOS/_GOARCH
// filename suffixes, exactly as the go command does (review a31a N1).
func defaultLaneBuilds(t *testing.T, path string) bool {
	t.Helper()
	ok, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		t.Fatalf("evaluate build constraints of %s: %v", path, err)
	}
	return ok
}

// declaredTests parses a _test.go file and returns its top-level TestXxx
// function names. A parse, not a regex, so a commented-out or string-literal
// `func TestX(` is not counted (review d7c2). A file that does not parse
// fails the law rather than being skipped.
func declaredTests(t *testing.T, path string, src []byte) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
			names = append(names, fn.Name.Name)
		}
	}
	return names
}

// testDeclarations walks the module's _test.go files and maps each top-level
// Test function name to the files declaring it. Hidden directories (.git,
// .claude worktrees) and vendor are skipped.
func testDeclarations(t *testing.T, root string) (map[string][]string, int) {
	t.Helper()
	decls := map[string][]string{}
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, p, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, p)
		// A file the default lane does not build (an integration tag, another
		// platform's suffix) does not gate an ordinary build: it is recorded
		// as gated and the checker refuses it as a pin.
		if !defaultLaneBuilds(t, p) {
			rel = tzGatedPrefix + rel
		}
		for _, name := range declaredTests(t, p, src) {
			decls[name] = append(decls[name], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return decls, files
}

func TestTrustZoneRegister_EveryInvariantNamesALiveTest(t *testing.T) {
	root := moduleRoot(t)
	page, err := os.ReadFile(filepath.Join(root, trustZonePublicPage))
	if err != nil {
		t.Fatalf("read %s: %v", trustZonePublicPage, err)
	}
	decls, files := testDeclarations(t, root)
	if files == 0 {
		t.Fatal("found no _test.go file: the walk reached nothing")
	}
	rows, problems := checkTrustZoneRegister(string(page), decls)
	pins := 0
	for _, r := range rows {
		pins += len(r.pins)
	}
	t.Logf("%s: %d invariants examined, %d test references resolved against %d test files",
		trustZonePublicPage, len(rows), pins, files)
	for _, p := range problems {
		t.Error(p)
	}

	// The EE design record carries the same register with the evidence; it
	// is absent from the CE tree, so its absence is not a failure there.
	lld, err := os.ReadFile(filepath.Join(root, trustZoneLLD))
	if os.IsNotExist(err) {
		// #334 item 6 (amendment 2026-10-14): absent is the CE export's
		// shape only; in the Enterprise tree it means half the law examined nothing.
		if enterpriseTree(root) {
			t.Fatalf("%s is absent from the Enterprise tree: the design-record half of the law examined nothing", trustZoneLLD)
		}
		t.Logf("%s absent (CE tree): public register checked alone", trustZoneLLD)
		return
	}
	if err != nil {
		t.Fatalf("read %s: %v", trustZoneLLD, err)
	}
	lldRows, lldProblems := checkTrustZoneRegister(string(lld), decls)
	for _, p := range lldProblems {
		t.Errorf("%s: %s", trustZoneLLD, p)
	}
	if p := registersDisagree(rows, lldRows); p != "" {
		t.Error(p)
	}
}

// registersDisagree compares the public register with the design record by
// id, status and pins, in order. It returns "" when they agree (review d7c2:
// a function, so the self-test can plant a disagreement).
func registersDisagree(public, design []tzRow) string {
	key := func(rs []tzRow) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.id+"="+r.status+"["+strings.Join(r.pins, " ")+"]")
		}
		return strings.Join(s, ",")
	}
	if key(public) == key(design) {
		return ""
	}
	return fmt.Sprintf("the public register and the design record disagree:\n public %s\n design %s", key(public), key(design))
}

// Self-test: the checker catches each failure shape it exists for, so a clean
// run above is not a parse that saw nothing. Each case names the problem it
// must produce, so a case cannot pass on an unrelated defect.
func TestTrustZoneRegister_CatchesEachViolationShape(t *testing.T) {
	decls := map[string][]string{"TestLive": {"a/b_test.go"}, "TestTwice": {"a/b_test.go", "c/d_test.go"},
		"TestGated": {tzGatedPrefix + "e/f_test.go"}}
	den := func(stated, pinned, refs int) string {
		return fmt.Sprintf("\n**%d invariants stated, %d pinned by at least one test (%d test references)**", stated, pinned, refs)
	}
	split := func(h, s, g int) string {
		return fmt.Sprintf(": %d hold as stated, %d hold only for the\nscope given in the row (**SCOPED**), and %d have a known gap", h, s, g)
	}
	row := "| TZ-1 | x | `TestLive` | HOLDS |"
	cases := []struct{ name, page, want string }{
		{"no table", "nothing here", "table is missing"},
		{"no test named", "| TZ-1 | x | none | HOLDS |" + den(1, 0, 0) + split(1, 0, 0), "names no test"},
		{"dead test", "| TZ-1 | x | `TestGone` | HOLDS |" + den(1, 1, 1) + split(1, 0, 0), "no _test.go file declares"},
		{"ambiguous test", "| TZ-1 | x | `TestTwice` | HOLDS |" + den(1, 1, 1) + split(1, 0, 0), "ambiguous"},
		{"gated test", "| TZ-1 | x | `TestGated` | HOLDS |" + den(1, 1, 1) + split(1, 0, 0), "does not run"},
		{"bad status", "| TZ-1 | x | `TestLive` | FINE |" + den(1, 1, 1) + split(0, 0, 0), "is not one of"},
		{"status not first", "| TZ-1 | x | `TestLive` | note: HOLDS |" + den(1, 1, 1) + split(0, 0, 0), "is not one of"},
		{"wrong count", row + den(2, 1, 1) + split(1, 0, 0), "published denominator"},
		{"wrong ref count", row + den(1, 1, 2) + split(1, 0, 0), "published denominator"},
		{"no denominator", row + split(1, 0, 0), "does not publish the denominator"},
		{"wrong split", row + den(1, 1, 1) + split(0, 1, 0), "published split"},
		{"no split", row + den(1, 1, 1), "does not publish the HOLDS/SCOPED/GAP split"},
		{"duplicate row id", row + "\n" + row + den(2, 2, 2) + split(2, 0, 0), "duplicate id"},
		{"pin only in prose", "| TZ-1 | `TestLive` | none | HOLDS |" + den(1, 0, 0) + split(1, 0, 0), "names no test"},
		{"short row", "| TZ-1 | x | `TestLive` |" + den(1, 1, 1) + split(1, 0, 0), "want 4"},
	}
	for _, c := range cases {
		_, problems := checkTrustZoneRegister(c.page, decls)
		if !strings.Contains(strings.Join(problems, "\n"), c.want) {
			t.Errorf("%s: want a problem containing %q, got %v", c.name, c.want, problems)
		}
	}
	clean := "| TZ-1 | x | `TestLive` | **SCOPED**: note naming `TestGone` |" + den(1, 1, 1) + split(0, 1, 0)
	if _, problems := checkTrustZoneRegister(clean, decls); len(problems) != 0 {
		t.Errorf("clean register reported problems: %v", problems)
	}
}

// The default-lane predicate builds what `go test` builds and nothing else
// (review a31a N1): a negated tag is built, a positive tag is not, and another
// platform's filename suffix is not.
func TestTrustZoneRegister_DefaultLanePredicate(t *testing.T) {
	dir := t.TempDir()
	write := func(name, header string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(header+"package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	other := "windows"
	if build.Default.GOOS == "windows" {
		other = "linux"
	}
	for _, c := range []struct {
		path string
		want bool
	}{
		{write("plain_test.go", ""), true},
		{write("neg_test.go", "//go:build !integration\n\n"), true},
		{write("pos_test.go", "//go:build integration\n\n"), false},
		{write("x_"+other+"_test.go", ""), false},
	} {
		if got := defaultLaneBuilds(t, c.path); got != c.want {
			t.Errorf("%s: default lane builds = %v, want %v", filepath.Base(c.path), got, c.want)
		}
	}
}

// The cross-register check reports a planted disagreement in a pin or a
// status, and is quiet when the registers agree (review d7c2 finding 1).
func TestTrustZoneRegister_RegistersDisagreeCatchesDrift(t *testing.T) {
	base := []tzRow{{id: "TZ-1", status: "HOLDS", pins: []string{"TestA", "TestB"}}}
	if p := registersDisagree(base, []tzRow{{id: "TZ-1", status: "HOLDS", pins: []string{"TestA", "TestB"}}}); p != "" {
		t.Errorf("agreeing registers reported: %s", p)
	}
	for name, design := range map[string][]tzRow{
		"pin differs":    {{id: "TZ-1", status: "HOLDS", pins: []string{"TestA", "TestC"}}},
		"status differs": {{id: "TZ-1", status: "GAP", pins: []string{"TestA", "TestB"}}},
		"row missing":    {},
	} {
		if registersDisagree(base, design) == "" {
			t.Errorf("%s: disagreement not reported", name)
		}
	}
}

// Only real top-level TestXxx declarations count: not a commented-out one,
// not one in a string, not a method (review d7c2 finding 3).
func TestTrustZoneRegister_DeclaredTestsParses(t *testing.T) {
	src := []byte("package x\n\nimport \"testing\"\n\n/*\nfunc TestFakeInComment(t *testing.T) {}\n*/\n\n" +
		"var s = `\nfunc TestFakeInString(t *testing.T) {}\n`\n\ntype r struct{}\n\nfunc (r) TestMethod() {}\n\n" +
		"func TestReal(t *testing.T) {}\n")
	got := declaredTests(t, "x_test.go", src)
	if len(got) != 1 || got[0] != "TestReal" {
		t.Errorf("declaredTests = %v, want [TestReal]", got)
	}
}

package architecture

import (
	"bufio"
	"bytes"
	"fmt"
	"go/build/constraint"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// The integration reach law (storage-abstraction design, "Which packages the
// integration tag reaches", change 2026-10-16, GitHub #393). A file behind
// //go:build integration is compiled only by a command that passes the tag,
// and the only such commands are the Postgres lane's: a tagged package outside
// the lane's package list is never built, never run, and reads as fine.
// internal/api's one tagged test stopped COMPILING (it declared func min,
// shadowing the builtin under the tag) and nothing noticed; internal/memory's
// Art 17 erasure suite ran in no lane until 2026-09-13.
//
// The law censuses every integration-tagged file by package and fails when:
//   - a tagged package is neither matched by the lane nor listed in
//     integrationReachExcluded with a reason;
//   - a listed package is in the lane, or no longer holds a tagged file;
//   - the copies of the lane's package list disagree (two make targets, the
//     EE and CE CI workflows, and integrationLaneDirs).
//
// It does not compile anything: `make lint-integration-vet` does
// (go vet -tags=integration ./...). Module-agnostic, so it ships in the
// Community export; there, a copy the export removed is skipped.

// integrationReachExcluded lists the integration-tagged packages no lane runs,
// each with the reason. make lint-integration-vet still compiles them.
var integrationReachExcluded = map[string]string{
	"internal/api": "its one tagged test (TestSkillCosineCalibration) measures cosine similarity against a live " +
		"embedding model (VORNIK_EMBED_ENDPOINT, VORNIK_EMBED_MODEL), a manual acceptance measurement of the skill " +
		"dedup preflight; no lane has an endpoint, so in the lane it would skip after rerunning the package's ~3,000 unit tests",
}

// laneCopy is one place the lane's package list is written.
type laneCopy struct {
	name     string   // where, for the message
	patterns []string // the ./… package patterns, sorted
	found    bool     // false when the file or target is absent
}

// integrationTagged reports whether src's build constraint is false without
// the integration tag and true with it, on this host's GOOS/GOARCH.
func integrationTagged(src []byte) (bool, error) {
	var expr constraint.Expr
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) && !constraint.IsPlusBuild(line) {
			continue
		}
		e, err := constraint.Parse(line)
		if err != nil {
			return false, err
		}
		if constraint.IsGoBuild(line) {
			expr = e // //go:build wins over // +build
			break
		}
		if expr == nil {
			expr = e
		}
	}
	if expr == nil {
		return false, nil
	}
	holds := func(integration bool) bool {
		return expr.Eval(func(tag string) bool {
			switch tag {
			case "integration":
				return integration
			case runtime.GOOS, runtime.GOARCH, "gc":
				return true
			case "unix":
				// The GOOS values go/build treats as unix (its unixOS list).
				switch runtime.GOOS {
				case "aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios",
					"linux", "netbsd", "openbsd", "solaris":
					return true
				}
			}
			return false
		})
	}
	return holds(true) && !holds(false), nil
}

// lanePatterns returns the ./… fields of a go test command line, sorted.
func lanePatterns(cmd string) []string {
	var out []string
	for _, f := range strings.Fields(cmd) {
		f = strings.Trim(f, `"'`)
		if strings.HasPrefix(f, "./") {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// makeTargetLane returns the patterns of the `go test … -tags=integration`
// command in Makefile target name; ok is false when the target is absent.
func makeTargetLane(makefile, name string) ([]string, bool, error) {
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, name+":") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, false, nil
	}
	var cmds []string
	var cur strings.Builder
	for _, l := range lines[start:] {
		if !strings.HasPrefix(l, "\t") {
			break
		}
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "@#") || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasSuffix(t, `\`) {
			cur.WriteString(strings.TrimSuffix(t, `\`) + " ")
			continue
		}
		cur.WriteString(t)
		cmds = append(cmds, cur.String())
		cur.Reset()
	}
	var got [][]string
	for _, c := range cmds {
		if strings.Contains(c, " test ") && strings.Contains(c, "-tags=integration") {
			got = append(got, lanePatterns(c))
		}
	}
	if len(got) != 1 {
		return nil, true, fmt.Errorf("target %s: %d go test -tags=integration commands, want exactly 1", name, len(got))
	}
	return got[0], true, nil
}

// workflowLane returns the patterns of the one `go test … -tags=integration`
// line of a CI workflow.
func workflowLane(src string) ([]string, error) {
	var got [][]string
	for _, l := range strings.Split(src, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			continue
		}
		// A trailing YAML comment is not part of the command.
		if i := strings.Index(t, " #"); i >= 0 {
			t = t[:i]
		}
		if strings.Contains(t, "go test") && strings.Contains(t, "-tags=integration") {
			got = append(got, lanePatterns(t))
		}
	}
	if len(got) != 1 {
		return nil, fmt.Errorf("%d go test -tags=integration lines, want exactly 1", len(got))
	}
	return got[0], nil
}

// laneMatches reports whether package directory dir is matched by pattern
// (./a/... matches a and below; ./a matches only a, as for go test: a pattern
// without /... names one package, not its subpackages).
func laneMatches(pattern, dir string) bool {
	p := strings.TrimPrefix(pattern, "./")
	if rest, ok := strings.CutSuffix(p, "/..."); ok {
		return dir == rest || strings.HasPrefix(dir, rest+"/")
	}
	if p == "..." {
		return true
	}
	return dir == path.Clean(p)
}

// integrationReach is the judgement, separated from the walk so planted
// inputs can drive it.
type integrationReach struct {
	tagged   map[string]int // package dir → tagged files
	copies   []laneCopy
	excluded map[string]string
	ee       bool
	dirGone  func(string) bool // a listed package the edition removed
}

func (r integrationReach) check() []string {
	var v []string
	var lane []string
	var laneFrom string
	nFound := 0
	for _, c := range r.copies {
		if !c.found {
			if r.ee {
				v = append(v, fmt.Sprintf("%s: no integration lane found there; the Enterprise tree must have every copy", c.name))
			}
			continue
		}
		nFound++
		if lane == nil {
			lane, laneFrom = c.patterns, c.name
			continue
		}
		if strings.Join(c.patterns, " ") != strings.Join(lane, " ") {
			v = append(v, fmt.Sprintf("the lane's package list differs: %s has %v, %s has %v; every copy must list the same packages",
				laneFrom, lane, c.name, c.patterns))
		}
	}
	if nFound == 0 {
		v = append(v, "no copy of the integration lane's package list was found; the law has nothing to judge against")
		return v
	}
	inLane := func(dir string) bool {
		for _, p := range lane {
			if laneMatches(p, dir) {
				return true
			}
		}
		return false
	}
	dirs := make([]string, 0, len(r.tagged))
	for d := range r.tagged {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		_, listed := r.excluded[d]
		if !inLane(d) && !listed {
			v = append(v, fmt.Sprintf("%s: %d integration-tagged file(s) that no lane runs; add the package to the integration lane "+
				"(every copy) or list it in integrationReachExcluded with the reason (#393)", d, r.tagged[d]))
		}
	}
	listedDirs := make([]string, 0, len(r.excluded))
	for d := range r.excluded {
		listedDirs = append(listedDirs, d)
	}
	sort.Strings(listedDirs)
	for _, d := range listedDirs {
		if strings.TrimSpace(r.excluded[d]) == "" {
			v = append(v, fmt.Sprintf("integrationReachExcluded[%q] has no reason", d))
		}
		switch {
		case inLane(d):
			v = append(v, fmt.Sprintf("integrationReachExcluded lists %s, which the lane runs: remove the stale entry", d))
		case r.tagged[d] == 0:
			if !r.ee && r.dirGone != nil && r.dirGone(d) {
				continue
			}
			v = append(v, fmt.Sprintf("integrationReachExcluded lists %s, which holds no integration-tagged file: remove the stale entry", d))
		}
	}
	return v
}

// readLaneCopies reads every copy of the lane's package list under root.
func readLaneCopies(root string) ([]laneCopy, error) {
	var copies []laneCopy
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, target := range []string{"test-integration", "test-coverage-integration"} {
		c := laneCopy{name: "Makefile target " + target}
		if err == nil {
			p, ok, perr := makeTargetLane(string(mk), target)
			if perr != nil {
				return nil, perr
			}
			c.patterns, c.found = p, ok
		}
		copies = append(copies, c)
	}
	for _, wf := range []string{".github/workflows/ci.yaml", "scripts/public-ce-templates/ci.yaml"} {
		c := laneCopy{name: wf}
		src, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(wf)))
		switch {
		case os.IsNotExist(rerr):
		case rerr != nil:
			return nil, rerr
		default:
			p, perr := workflowLane(string(src))
			if perr != nil {
				return nil, fmt.Errorf("%s: %w", wf, perr)
			}
			c.patterns, c.found = p, true
		}
		copies = append(copies, c)
	}
	dirs := make([]string, 0, len(integrationLaneDirs))
	for _, d := range integrationLaneDirs {
		dirs = append(dirs, "./"+d+"/...")
	}
	sort.Strings(dirs)
	copies = append(copies, laneCopy{name: "integrationLaneDirs (integration_page_law_test.go)", patterns: dirs, found: true})
	return copies, nil
}

// censusIntegrationTagged counts the integration-tagged .go files under root by
// package directory.
func censusIntegrationTagged(root string) (map[string]int, error) {
	tagged := map[string]int{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(root, p, d.Name(), "testdata") {
				return filepath.SkipDir
			}
			// A nested module is not built by the root module's lane.
			if p != root {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		ok, err := integrationTagged(src)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		tagged[filepath.ToSlash(rel)]++
		return nil
	})
	return tagged, err
}

func integrationReachLaw(t testing.TB, root string) {
	t.Helper()
	tagged, err := censusIntegrationTagged(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	copies, err := readLaneCopies(root)
	if err != nil {
		t.Fatalf("lane: %v", err)
	}
	r := integrationReach{
		tagged:   tagged,
		copies:   copies,
		excluded: integrationReachExcluded,
		ee:       enterpriseTree(root),
		dirGone: func(d string) bool {
			_, err := os.Stat(filepath.Join(root, filepath.FromSlash(d)))
			return os.IsNotExist(err)
		},
	}
	files, laneFiles, laneDirs, exFiles, exDirs := 0, 0, 0, 0, 0
	var lane []string
	for _, c := range copies {
		if c.found {
			lane = c.patterns
			break
		}
	}
	for d, n := range tagged {
		files += n
		in := false
		for _, p := range lane {
			if laneMatches(p, d) {
				in = true
				break
			}
		}
		switch {
		case in:
			laneFiles += n
			laneDirs++
		case integrationReachExcluded[d] != "":
			exFiles += n
			exDirs++
		}
	}
	t.Logf("integration reach law: %d integration-tagged files in %d packages; %d files in %d packages run by the lane %v; "+
		"%d files in %d packages excluded with a reason; the rest reach no lane", files, len(tagged), laneFiles, laneDirs, lane, exFiles, exDirs)
	if !requireExamined(t, "integration reach law", "tagged files", files, reachFloorFiles) ||
		!requireExamined(t, "integration reach law", "tagged packages", len(tagged), reachFloorPackages) {
		return
	}
	for _, v := range r.check() {
		t.Error(v)
	}
}

// The floors sit well under the 2026-10-16 census (104 files in 5 packages)
// and above zero, so a walk that lost the tagged files fails rather than passes.
const (
	reachFloorFiles    = 50
	reachFloorPackages = 3
)

func TestIntegrationReachLaw(t *testing.T) {
	integrationReachLaw(t, moduleRoot(t))
}

// The law's own tests: planted inputs prove each rule fires.

func TestIntegrationReachLaw_Tagged(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"//go:build integration\n\npackage p\n", true},
		{"//go:build integration && " + runtime.GOOS + "\n\npackage p\n", true},
		{"//go:build integration || e2e\n\npackage p\n", true},
		{"// +build integration\n\npackage p\n", true},
		{"//go:build !integration\n\npackage p\n", false},
		{"//go:build e2e\n\npackage p\n", false},
		{"//go:build " + runtime.GOOS + "\n\npackage p\n", false},
		{"package p\n\n//go:build integration\n", false},
		{"// Package p.\npackage p\n", false},
	}
	for _, c := range cases {
		got, err := integrationTagged([]byte(c.src))
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if got != c.want {
			t.Errorf("integrationTagged(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestIntegrationReachLaw_ReadsTheMakeTarget(t *testing.T) {
	mk := "x:\n\techo\n\n## test-integration: doc\ntest-integration:\n\t@# ./internal/zzz/... in a comment is not the lane\n" +
		"\tscripts/integration-db-lane.sh \\\n\t$(GO) test $(TEST_FLAGS) -p 1 -tags=integration \\\n\t\t-coverpkg=./... \\\n\t\t./b/... ./a\n\nnext:\n\t$(GO) test -tags=integration ./c\n"
	got, ok, err := makeTargetLane(mk, "test-integration")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if strings.Join(got, " ") != "./a ./b/..." {
		t.Errorf("patterns = %v, want [./a ./b/...]", got)
	}
	if _, ok, _ := makeTargetLane(mk, "test-coverage-integration"); ok {
		t.Error("an absent target must report not found")
	}
}

func TestIntegrationReachLaw_ReadsTheWorkflow(t *testing.T) {
	src := "      # go test -tags=integration ./commented/...\n      run: go test -p 1 -tags=integration -count=1 ./x/... ./y/... # not ./z\n"
	got, err := workflowLane(src)
	if err != nil || strings.Join(got, " ") != "./x/... ./y/..." {
		t.Fatalf("got %v err %v", got, err)
	}
	if _, err := workflowLane(src + "      run: go test -tags=integration ./z\n"); err == nil {
		t.Error("two integration lines must fail: the law cannot tell which is the lane")
	}
}

func TestIntegrationReachLaw_Check(t *testing.T) {
	lane := []string{"./a/...", "./b"}
	same := func(names ...string) []laneCopy {
		var cs []laneCopy
		for _, n := range names {
			cs = append(cs, laneCopy{name: n, patterns: lane, found: true})
		}
		return cs
	}
	has := func(vs []string, sub string) bool {
		for _, v := range vs {
			if strings.Contains(v, sub) {
				return true
			}
		}
		return false
	}

	// #393: a tagged package outside the lane and unlisted fails.
	r := integrationReach{tagged: map[string]int{"a/x": 2, "b": 1, "c": 1}, copies: same("m", "w"), excluded: map[string]string{}, ee: true}
	if v := r.check(); len(v) != 1 || !has(v, "c: 1 integration-tagged file") {
		t.Errorf("unreached package: %q", v)
	}
	// ./b does not match b/sub.
	r.tagged = map[string]int{"b/sub": 1}
	if v := r.check(); !has(v, "b/sub") {
		t.Errorf("./b must not match b/sub: %q", v)
	}
	// Listed with a reason passes.
	r.tagged = map[string]int{"a": 1, "c": 1}
	r.excluded = map[string]string{"c": "needs a model"}
	if v := r.check(); len(v) != 0 {
		t.Errorf("listed package: %q", v)
	}
	// A listing without a reason fails.
	r.excluded = map[string]string{"c": " "}
	if v := r.check(); !has(v, "no reason") {
		t.Errorf("empty reason: %q", v)
	}
	// Stale: listed but in the lane, or no longer tagged.
	r.excluded = map[string]string{"a/y": "r", "d": "r"}
	r.tagged = map[string]int{"a/y": 1}
	if v := r.check(); !has(v, "a/y, which the lane runs") || !has(v, "d, which holds no integration-tagged file") {
		t.Errorf("stale listings: %q", v)
	}
	// In the Community export, a listed package the export removed is skipped.
	r.ee = false
	r.dirGone = func(d string) bool { return d == "d" }
	if v := r.check(); has(v, "lists d,") {
		t.Errorf("CE removed package must be skipped: %q", v)
	}
	// Copies that disagree fail.
	r = integrationReach{tagged: map[string]int{"a": 1}, excluded: map[string]string{}, ee: true,
		copies: append(same("m"), laneCopy{name: "w", patterns: []string{"./a/..."}, found: true})}
	if v := r.check(); !has(v, "differs") {
		t.Errorf("disagreeing copies: %q", v)
	}
	// An absent copy fails in the Enterprise tree, is skipped in the export.
	r.copies = append(same("m"), laneCopy{name: "w"})
	if v := r.check(); !has(v, "w: no integration lane found") {
		t.Errorf("EE absent copy: %q", v)
	}
	r.ee = false
	if v := r.check(); len(v) != 0 {
		t.Errorf("CE absent copy: %q", v)
	}
	// No copy at all fails everywhere.
	r.copies = []laneCopy{{name: "m"}}
	if v := r.check(); !has(v, "no copy") {
		t.Errorf("no copy: %q", v)
	}
}

func TestIntegrationReachLaw_LaneMatches(t *testing.T) {
	for _, c := range []struct {
		p, d string
		want bool
	}{
		{"./a/...", "a", true}, {"./a/...", "a/b/c", true}, {"./a/...", "ab", false},
		{"./a", "a", true}, {"./a", "a/b", false}, {"./...", "anything", true},
	} {
		if got := laneMatches(c.p, c.d); got != c.want {
			t.Errorf("laneMatches(%q, %q) = %v, want %v", c.p, c.d, got, c.want)
		}
	}
}

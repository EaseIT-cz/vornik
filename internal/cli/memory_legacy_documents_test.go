package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// Legacy document cleanup (memory rollback x supersession design, amendment
// A.5). Incident: re-ingests never superseded earlier versions, so the store
// holds many bare-name versions of each document; this verb retires them
// once the document has been re-ingested under its path, and never touches a
// name that is ambiguous in the checkout.

func TestPlanLegacyDocuments(t *testing.T) {
	docs := []memory.LegacyDocument{
		{Name: "reference-architecture.md", Chunks: 7, ByDate: map[string]int{"2026-09-01": 3, "2026-09-20": 4}},
		{Name: "index.md", Chunks: 2},
		{Name: "gone.md", Chunks: 1},
		{Name: "backlog.md", Chunks: 9},
	}
	tracked := []string{
		"docs/guides/reference-architecture.md",
		"docs/public/index.md", "docs/public/guides/index.md",
		"docs/guides/backlog.md",
	}
	survivors := map[string]string{"docs/guides/reference-architecture.md": "art_new"}
	rows, err := planLegacyDocumentsWith(docs, tracked, func(p string) (string, error) { return survivors[p], nil }, wholeVersion)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]legacyPlanRow{}
	for _, r := range rows {
		got[r.Name] = r
	}
	if r := got["reference-architecture.md"]; r.Decision != legacySupersede || r.Path != "docs/guides/reference-architecture.md" || r.Survivor != "art_new" {
		t.Errorf("unique and re-ingested must be superseded: %+v", r)
	}
	if r := got["index.md"]; r.Decision != legacyAmbiguous || len(r.Candidates) != 2 {
		t.Errorf("two files share the name, so it must be left alone: %+v", r)
	}
	if r := got["gone.md"]; r.Decision != legacyMissing {
		t.Errorf("a name not in the checkout must be left alone: %+v", r)
	}
	if r := got["backlog.md"]; r.Decision != legacyNotReingested || r.Path != "docs/guides/backlog.md" {
		t.Errorf("a name whose path has no live version yet must be left alone: %+v", r)
	}
}

func TestPlanLegacyDocuments_SurvivorErrorStops(t *testing.T) {
	_, err := planLegacyDocumentsWith([]memory.LegacyDocument{{Name: "a.md"}}, []string{"a.md"},
		func(string) (string, error) { return "", errors.New("db down") }, wholeVersion)
	if err == nil {
		t.Fatal("a failed lookup must stop the plan, not read as 'not re-ingested'")
	}
}

func TestRenderLegacyPlan_PrintsBlastRadiusAndDenominator(t *testing.T) {
	rows := []legacyPlanRow{
		{Name: "a.md", Chunks: 7, ByDate: map[string]int{"2026-09-01": 3, "2026-09-20": 4}, Decision: legacySupersede, Path: "docs/a.md", Survivor: "art_new"},
		{Name: "index.md", Chunks: 2, Decision: legacyAmbiguous, Candidates: []string{"x/index.md", "y/index.md"}},
		{Name: "gone.md", Chunks: 1, Decision: legacyMissing},
		{Name: "b.md", Chunks: 9, Decision: legacyNotReingested, Path: "docs/b.md"},
	}
	out := renderLegacyPlan(rows)
	for _, want := range []string{
		"a.md -> docs/a.md", "survivor art_new", "2026-09-01: 3", "2026-09-20: 4",
		"x/index.md", "y/index.md",
		"examined 4 names: supersede 1 (7 chunks), keep newest 0 (0 chunks), incomplete 0, ambiguous 1, missing from the checkout 1, not yet re-ingested 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestTrackedFiles(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, "docs/a.md", "a")
	commitFile(t, dir, "docs/sub/b.md", "b")
	files, err := trackedFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(files, ",")
	if !strings.Contains(joined, "docs/a.md") || !strings.Contains(joined, "docs/sub/b.md") {
		t.Fatalf("tracked files = %v", files)
	}
	if _, err := trackedFiles(t.TempDir()); err == nil {
		t.Fatal("a directory that is not a git checkout must be an error")
	}
}

// A file at the repository root has a path equal to its bare name, so its
// legacy chunks ARE its path-identity version. Retiring every chunk of the name
// would delete the document; only the older uploads may go. Found by the dry run
// against the reference host's store on 2026-09-26 (CLAUDE.md, EULA.md,
// RELEASE.md, TRADEMARKS.md were each planned for full retirement).
func TestPlanLegacyDocuments_RootFileKeepsItsNewestUpload(t *testing.T) {
	docs := []memory.LegacyDocument{{Name: "RELEASE.md", Chunks: 37}}
	rows, err := planLegacyDocumentsWith(docs, []string{"RELEASE.md"}, func(string) (string, error) { return "art_newest", nil }, wholeVersion)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Decision != legacyKeepNewest || rows[0].Survivor != "art_newest" {
		t.Fatalf("a root file must keep its newest upload, got %+v", rows[0])
	}
}

func TestRenderLegacyPlan_RootFileReportsOnlyTheOlderChunks(t *testing.T) {
	out := renderLegacyPlan([]legacyPlanRow{
		{Name: "RELEASE.md", Chunks: 37, Decision: legacyKeepNewest, Path: "RELEASE.md", Survivor: "art_newest", Retire: 30},
	})
	for _, want := range []string{"RELEASE.md", "keeps art_newest", "30 older chunks", "keep newest 1 (30 chunks)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// The checkout's scope, derived the way the companion plugin derives it, so
// --root and --scope cannot silently describe two different repositories
// (code review of 2026-09-26, client side, finding 3).
func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/widgets.git":     "github.com/acme/widgets",
		"git@github.com:acme/widgets.git":         "github.com/acme/widgets",
		"ssh://git@github.com/acme/widgets.git":   "github.com/acme/widgets",
		"https://user@gitlab.example.com/a/b.git": "gitlab.example.com/a/b",
		"github.com/acme/widgets":                 "github.com/acme/widgets",
	} {
		if got := normalizeRemote(in); got != want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckoutScope(t *testing.T) {
	dir := gitRepo(t)
	if got, err := checkoutScope(dir); err != nil || got != filepath.Base(dir) {
		t.Fatalf("no remote: scope = %q (%v), want the folder name", got, err)
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "git@github.com:acme/widgets.git").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if got, err := checkoutScope(dir); err != nil || got != "github.com/acme/widgets" {
		t.Fatalf("scope = %q (%v), want github.com/acme/widgets", got, err)
	}
}

// Files /rag-ingest can send a path for are the checkout's working tree, not
// only its index, so an uncommitted document is still retire-eligible; ignored
// files are not (client review, finding 2).
func TestTrackedFiles_IncludesUntrackedButNotIgnored(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, ".gitignore", "build/\n")
	for rel, body := range map[string]string{"docs/new.md": "n", "build/out.md": "o"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := trackedFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(files, ",")
	if !strings.Contains(joined, "docs/new.md") || strings.Contains(joined, "build/out.md") {
		t.Fatalf("files = %v, want the uncommitted doc and not the ignored one", files)
	}
}

// An ambiguous name reports which candidates are live under their paths, so
// the operator can act on it by hand (client review, finding 7).
func TestPlanLegacyDocuments_AmbiguousReportsLiveCandidates(t *testing.T) {
	rows, err := planLegacyDocumentsWith([]memory.LegacyDocument{{Name: "index.md", Chunks: 2}},
		[]string{"a/index.md", "b/index.md"},
		func(p string) (string, error) {
			if p == "a/index.md" {
				return "art_a", nil
			}
			return "", nil
		}, wholeVersion)
	if err != nil {
		t.Fatal(err)
	}
	out := renderLegacyPlan(rows)
	if !strings.Contains(out, "live under their path: a/index.md") {
		t.Fatalf("report does not name the live candidate:\n%s", out)
	}
}

// A.7 (review N3): a name whose path version predates A.7 is left alone, since
// that version may be missing sections that only the bare-name chunks hold.
func TestPlanLegacyDocuments_IncompletePathVersionIsLeftAlone(t *testing.T) {
	rows, err := planLegacyDocumentsWith([]memory.LegacyDocument{{Name: "a.md", Chunks: 3}, {Name: "RELEASE.md", Chunks: 5}},
		[]string{"docs/a.md", "RELEASE.md"},
		func(p string) (string, error) { return "art_" + p, nil },
		func(string) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Decision != legacyIncomplete {
			t.Errorf("%s: decision %s, want %s", r.Name, r.Decision, legacyIncomplete)
		}
	}
	if out := renderLegacyPlan(rows); !strings.Contains(out, "incomplete 2") || !strings.Contains(out, "re-ingest") {
		t.Errorf("report does not count or explain incomplete names:\n%s", out)
	}
}

// wholeVersion stands in for a path version stored whole (design A.7).
func wholeVersion(string) (bool, error) { return true, nil }

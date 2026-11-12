package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// GitHub #701 (memory rollback x supersession design, A.8). Found at
// production: deleted and moved documents stayed in recall (347 chunks across
// 32 old paths). The verb retires a path document whose path is no longer in
// the checkout. Lower because: the verb's inputs (a tracked set and a store)
// are built here exactly as the operator's.

func TestPlanMissingDocuments_701(t *testing.T) {
	docs := []memory.LegacyDocument{
		{Name: "kb/kept.md", Chunks: 2},
		{Name: "./kb/dotted.md", Chunks: 1},
		{Name: "kb/old/gone.md", Chunks: 4},
		{Name: "Kb/case.md", Chunks: 3},
		{Name: "kb/old/moved.md", Chunks: 5},
	}
	tracked := []string{"kb/kept.md", "kb/dotted.md", "kb/case.md", "kb/archive/moved.md", "guides/moved.md"}
	live := map[string]string{"kb/old/gone.md": "art-gone", "Kb/case.md": "art-case", "kb/old/moved.md": "art-moved",
		"kb/archive/moved.md": "art-archive", "guides/moved.md": ""}
	rows, err := planMissingDocumentsWith(docs, tracked, func(p string) (string, error) { return live[p], nil })
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]missingPlanRow{}
	for _, r := range rows {
		got[r.Path] = r
	}
	for path, want := range map[string]string{
		"kb/kept.md": missingPresent, "./kb/dotted.md": missingPresent,
		"kb/old/gone.md": missingMissing, "Kb/case.md": missingMissing, "kb/old/moved.md": missingMissing,
	} {
		if got[path].Decision != want {
			t.Errorf("%s: decision %q, want %q", path, got[path].Decision, want)
		}
	}
	if r := got["kb/old/moved.md"]; r.Survivor != "art-moved" || strings.Join(r.MovedTo, ",") != "kb/archive/moved.md" {
		t.Errorf("moved row = %+v: want the survivor recorded and only the live same-name path as a note", r)
	}
	out := renderMissingPlan(rows)
	if !strings.Contains(out, "examined 5 paths: 2 present, 3 missing (12 chunks)") {
		t.Errorf("render lacks the denominator:\n%s", out)
	}
	if !strings.Contains(out, "moved to? kb/archive/moved.md") {
		t.Errorf("render lacks the moved-to note:\n%s", out)
	}
}

func TestOnDefaultBranch_701(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.md")
	run("commit", "-qm", "a")
	if err := onDefaultBranch(dir); err == nil {
		t.Fatal("a checkout with no origin/HEAD was accepted")
	}
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	run("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	if err := onDefaultBranch(dir); err != nil {
		t.Fatalf("main against origin/HEAD = origin/main: %v", err)
	}
	run("checkout", "-q", "--detach")
	if err := onDefaultBranch(dir); err == nil {
		t.Fatal("a detached HEAD was accepted")
	}
	run("checkout", "-q", "main")
	run("checkout", "-qb", "feature")
	if err := onDefaultBranch(dir); err == nil || !strings.Contains(err.Error(), "feature") {
		t.Fatalf("a feature branch was accepted: %v", err)
	}
}

func TestMissingDocumentStillAsPlanned_701(t *testing.T) {
	row := missingPlanRow{Path: "kb/old/gone.md", Decision: missingMissing, Survivor: "art-1"}
	if !stillAsPlanned(row, "art-1") {
		t.Fatal("an unchanged survivor must be retired")
	}
	if stillAsPlanned(row, "art-2") || stillAsPlanned(row, "") {
		t.Fatal("a path re-ingested (or already retired) since the plan must be skipped")
	}
}

func TestApplyMissingPlan_701(t *testing.T) {
	rows := []missingPlanRow{
		{Path: "kb/a.md", Decision: missingMissing, Survivor: "art-a"},
		{Path: "kb/b.md", Decision: missingMissing, Survivor: "art-b"},
		{Path: "kb/c.md", Decision: missingPresent},
	}
	now := map[string]string{"kb/a.md": "art-a", "kb/b.md": "art-b2"} // b was re-ingested since the plan
	var retired []string
	var out strings.Builder
	total, paths, skipped, err := applyMissingPlan(rows, func(p string) (string, error) { return now[p], nil },
		func(p string) (int, error) { retired = append(retired, p); return 4, nil }, &out)
	if err != nil || total != 4 || paths != 1 || skipped != 1 || strings.Join(retired, ",") != "kb/a.md" {
		t.Fatalf("total %d paths %d skipped %d err %v retired %v", total, paths, skipped, err, retired)
	}
	if !strings.Contains(out.String(), "skipped  kb/b.md") {
		t.Fatalf("output: %s", out.String())
	}
}

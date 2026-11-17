package memory

// GitHub #669 item 4 (design 22, §22.2.16b): a configured provenance clone
// carries a marker saying the daemon depends on it, kept out of git status.
// On 2026-10-28 a configured clone was deleted by hand; nothing in it said
// the daemon used it. Found at: production. Regression test at: unit (a
// real git clone and the sweep's fetch). Lower because the deletion is a
// person's act, which no lane reproduces; the test pins what they would see.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func countLines(t *testing.T, path, line string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l == line {
			n++
		}
	}
	return n
}

func TestProvenanceCloneMarker669_WrittenOnceAndExcluded(t *testing.T) {
	_, clone, scope, v := daemonCloneFixture(t)
	s, _ := newFetchSweeper(v, clone, scope)
	s.FetchClones(context.Background())
	marker := filepath.Join(clone, ProvenanceCloneMarker)
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no marker after a successful fetch: %v", err)
	}
	for _, want := range []string{"proj", "memory.provenance_clone"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the marker does not name %q:\n%s", want, b)
		}
	}
	exclude := filepath.Join(clone, ".git", "info", "exclude")
	if n := countLines(t, exclude, "/"+ProvenanceCloneMarker); n != 1 {
		t.Errorf("exclude lists the marker %d times, want 1", n)
	}
	if out := provGit(t, clone, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("git status is not clean with the marker: %q", out)
	}
	// A second sweep adds neither again, and an existing marker is left alone.
	if err := os.WriteFile(marker, []byte("operator note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.FetchClones(context.Background())
	if n := countLines(t, exclude, "/"+ProvenanceCloneMarker); n != 1 {
		t.Errorf("after a second sweep exclude lists the marker %d times, want 1", n)
	}
	if b, _ := os.ReadFile(marker); string(b) != "operator note\n" {
		t.Errorf("an existing marker was rewritten: %q", b)
	}
}

// A clone whose .git is a file (a worktree, a submodule) is refused by the
// verifier's clone checks today (unsafe_clone), so the sweep never marks
// it; markClone keeps the tree clean on its own too, should that change.
func TestProvenanceCloneMarker669_GitFileCloneGetsNoMarker(t *testing.T) {
	_, clone, scope, v := daemonCloneFixture(t)
	wt := filepath.Join(filepath.Dir(clone), "wt")
	provGit(t, clone, "worktree", "add", "-q", wt)
	v.CloneDir = func(string) string { return wt }
	s, _ := newFetchSweeper(v, wt, scope)
	s.FetchClones(context.Background())
	s.markClone(wt, "proj")
	if _, err := os.Stat(filepath.Join(wt, ProvenanceCloneMarker)); !os.IsNotExist(err) {
		t.Errorf("a clone whose .git is a file got a marker (%v)", err)
	}
	if out := provGit(t, wt, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("git status is not clean: %q", out)
	}
}

func TestProvenanceCloneMarker669_RefusedCloneGetsNoMarker(t *testing.T) {
	_, clone, _, v := daemonCloneFixture(t)
	s, _ := newFetchSweeper(v, clone, "github.com/someone/else")
	s.FetchClones(context.Background())
	if _, err := os.Stat(filepath.Join(clone, ProvenanceCloneMarker)); !os.IsNotExist(err) {
		t.Errorf("a refused clone got a marker (%v)", err)
	}
}

func TestProvenanceCloneMarker669_WriteFailureChangesNothingElse(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode")
	}
	_, clone, scope, v := daemonCloneFixture(t)
	if err := os.Chmod(clone, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(clone, 0o755) })
	s, _ := newFetchSweeper(v, clone, scope)
	s.FetchClones(context.Background())
	if got := s.LastFetches(); len(got) != 1 || got[0].Outcome != ProvenanceFetchOK {
		t.Errorf("a marker write failure changed the fetch outcome: %+v", got)
	}
}

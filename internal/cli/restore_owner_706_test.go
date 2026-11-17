package cli

// GitHub #706 (enterprise-packaging-design.md amendment 2026-11-14): a
// restore run as root gives the restored trees to the owner of their
// destination's parent, the destination itself included, and touches only
// what the copy wrote; run as another user it changes no owner. Before, a
// root restore left /var/lib/vornik/artifacts root's, hidden only by the
// start-time chown walk #706 removes. Found at: design review of #706
// (would be observed at e2e-lane, a managed rollback). Regression test at:
// unit. Lower because the managed rollback runs in vornik-cloud's upgrade
// lane, not in this repository.

import (
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
)

type chownCall struct {
	path     string
	uid, gid int
}

func withRestoreSeams(t *testing.T, euid int) *[]chownCall {
	t.Helper()
	var calls []chownCall
	oldE, oldC := restoreEUID, restoreLchown
	restoreEUID = func() int { return euid }
	restoreLchown = func(p string, uid, gid int) error {
		calls = append(calls, chownCall{p, uid, gid})
		return nil
	}
	t.Cleanup(func() { restoreEUID, restoreLchown = oldE, oldC })
	return &calls
}

func restoreFixture(t *testing.T) (src, parent, dst string) {
	t.Helper()
	root := t.TempDir()
	src = filepath.Join(root, "staging", "artifacts")
	for _, d := range []string{"a", "a/b"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"top.txt", "a/b/deep.txt"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("top.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	parent = filepath.Join(root, "var-lib-vornik")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	dst = filepath.Join(parent, "artifacts")
	// A file already in the destination that the copy does not write.
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "preexisting.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	return src, parent, dst
}

func TestRestoreTree706_RootGivesCopiedEntriesToTheParentsOwner(t *testing.T) {
	calls := withRestoreSeams(t, 0)
	src, parent, dst := restoreFixture(t)
	if err := restoreTree(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	var got []string
	for _, c := range *calls {
		if c.uid != int(st.Uid) || c.gid != int(st.Gid) {
			t.Errorf("%s chowned to %d:%d, want the parent's %d:%d", c.path, c.uid, c.gid, st.Uid, st.Gid)
		}
		rel, _ := filepath.Rel(dst, c.path)
		got = append(got, rel)
	}
	sort.Strings(got)
	want := []string{".", "a", "a/b", "a/b/deep.txt", "top.txt"}
	if len(got) != len(want) {
		t.Fatalf("chowned %v, want %v (the destination, its copied dirs and files; no link, nothing pre-existing)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chowned %v, want %v", got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "a/b/deep.txt")); err != nil {
		t.Fatalf("the copy did not happen: %v", err)
	}
	// What the copy wrote: no link (archiveutil.CopyDir skips them), so
	// skipping links in the chown leaves nothing root-owned behind.
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Errorf("the copy wrote the link (%v); restoreTree would leave it root-owned", err)
	}
}

func TestRestoreTree706_NonRootChownsNothing(t *testing.T) {
	calls := withRestoreSeams(t, 1000)
	src, _, dst := restoreFixture(t)
	if err := restoreTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Errorf("a non-root restore chowned %d entries, want none", len(*calls))
	}
	if _, err := os.Stat(filepath.Join(dst, "top.txt")); err != nil {
		t.Fatalf("the copy did not happen: %v", err)
	}
}

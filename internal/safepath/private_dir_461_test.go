//go:build unix

package safepath

// GitHub #461 (support-report design, addendum 2026-11-05). Found at: review
// (#427's lane, 2026-10-17): fixed-name roots under the temp dir were created
// with os.MkdirAll, which accepts a directory another user planted and
// follows a planted symlink.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsurePrivateDir_CreatesMissing_461(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b")
	if err := EnsurePrivateDir(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("want a 0700 directory, got %v %v", fi, err)
	}
	if err := EnsurePrivateDir(p); err != nil {
		t.Fatalf("an existing private directory must pass: %v", err)
	}
}

func TestEnsurePrivateDir_RefusesSymlink_461(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "vornik-root")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(link); err == nil {
		t.Fatal("a symlink at the path must be refused")
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Fatal("nothing may be written through the symlink")
	}
}

func TestEnsurePrivateDir_RefusesWritableByOthers_461(t *testing.T) {
	p := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(p); err == nil {
		t.Fatal("a directory others can write must be refused")
	}
}

func TestEnsurePrivateDir_RefusesAFile_461(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(p); err == nil {
		t.Fatal("a file at the path must be refused")
	}
}

// Outside the temp dir (a configured data or artifacts directory) only the
// creation applies: its owner may be another account (review 0ca6 F2).
func TestEnsurePrivateDir_OutsideTempDirNotChecked_461(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	if err := os.Mkdir(filepath.Join(base, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(data, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(data); err != nil {
		t.Fatalf("a configured directory outside the temp dir must pass: %v", err)
	}
	if err := EnsurePrivateDir(filepath.Join(data, "new")); err != nil {
		t.Fatalf("and missing ones are created: %v", err)
	}
}

func TestUnderTempDir_461(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	for path, want := range map[string]bool{
		base:                               false, // the shared root itself
		filepath.Join(base, "vornik-x"):    true,
		filepath.Join(base, "a", ".."):     false,
		filepath.Join(base, "..", "other"): false,
	} {
		if got := underTempDir(path); got != want {
			t.Errorf("underTempDir(%q) = %v, want %v", path, got, want)
		}
	}
}

// A symlink planted at a missing parent between the temp dir and the path is
// refused too, not followed by MkdirAll (automated security review,
// 2026-11-05: the check covered only the final component).
func TestEnsurePrivateDir_RefusesSymlinkedParent_461(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "planted")); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(filepath.Join(base, "planted", "root")); err == nil {
		t.Fatal("a symlinked parent must be refused")
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Fatal("nothing may be created through the symlinked parent")
	}
}

// A parent others can write is refused: they could swap the child.
func TestEnsurePrivateDir_RefusesWritableParent_461(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(filepath.Join(parent, "root")); err == nil {
		t.Fatal("a parent others can write must be refused")
	}
}

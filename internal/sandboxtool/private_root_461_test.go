//go:build unix

package sandboxtool

// GitHub #461: the scratch root, by default a fixed name under the temp dir,
// must refuse a planted symlink. Found at: review (2026-10-17).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScratchRefusesPlantedSymlink_461(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "planted")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "vornik-sandbox-tools")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{ScratchRoot: root}}
	if _, _, _, err := r.scratch(nil, 0); err == nil {
		t.Fatal("a symlinked scratch root must be refused")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatal("nothing may be written through the planted symlink")
	}
}

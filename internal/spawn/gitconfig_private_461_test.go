//go:build unix

package spawn

// GitHub #461: the git-config data dir, by default a fixed name under the
// temp dir, must refuse a planted symlink. Found at: review (2026-10-17).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestComposeGitConfigRefusesPlantedSymlink_461(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "planted")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(base, "vornik-1000")
	if err := os.Symlink(target, dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := ComposeGitConfig(context.Background(), dataDir); err == nil {
		t.Fatal("a symlinked data dir must be refused")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatal("nothing may be written through the planted symlink")
	}
}

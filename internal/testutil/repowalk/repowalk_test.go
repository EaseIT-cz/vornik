package repowalk

import (
	"path/filepath"
	"testing"
)

// Release 2026.10.6, 2026-10-14: publish-ce runs the exported tree's tests
// inside $SRC/.vornik-export. Four tree-walking laws skipped every
// dot-directory, their own root included, examined nothing there and failed
// the publish (TestFakeAgentStaysDeleted and three ChannelReceiver laws). The
// same shape broke 2026.9.0's publish (imagemanifest, 275a97ca7).
func TestSkipDir_NeverSkipsTheRoot(t *testing.T) {
	for _, root := range []string{"/src/.vornik-export", "/src/.vornik-public-clone", "/src/repo", "../.."} {
		if SkipDir(root, root, filepath.Base(root)) {
			t.Errorf("SkipDir(%q) skipped the walk root", root)
		}
	}
}

func TestSkipDir_SkipsHiddenVendorAndNodeModulesBelowTheRoot(t *testing.T) {
	root := "/src/.vornik-export"
	for _, name := range []string{".git", ".claude", ".vornik-export", ".vornik-public-clone", ".hidden", "vendor", "node_modules"} {
		if !SkipDir(root, filepath.Join(root, "x", name), name) {
			t.Errorf("SkipDir(%s) = false below the root, want true", name)
		}
	}
	for _, name := range []string{"internal", "cmd", "testdata", "e2e"} {
		if SkipDir(root, filepath.Join(root, name), name) {
			t.Errorf("SkipDir(%s) = true, want false", name)
		}
	}
}

package executor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestWorktreeInUseByContainer_EmptyPathFalse — the empty-path short
// circuit avoids touching podman.
func TestWorktreeInUseByContainer_EmptyPathFalse(t *testing.T) {
	assert.False(t, worktreeInUseByContainer(context.Background(), ""))
}

// TestWorktreeInUseByContainer_NoPodmanReturnsFalse — exec failure
// (podman not on PATH, container error) is documented as "no" so
// the cleanup path doesn't get stuck if podman is misconfigured.
func TestWorktreeInUseByContainer_NoPodmanReturnsFalse(t *testing.T) {
	t.Setenv("PATH", "") // make exec.LookPath fail for podman
	assert.False(t, worktreeInUseByContainer(context.Background(), "/some/wt"))
}

// TestProjectCleanExcludeDir_DefaultsAndRejections — pin every
// branch of the small helper.
func TestProjectCleanExcludeDir_AllBranches(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"absolute rejected", "/etc/notes.md", ""},
		{"parent traversal rejected", "../escape.md", ""},
		{"deeper traversal rejected", "../../etc/passwd", ""},
		{"root-level file returns empty", "PROJECT_CONTEXT.md", ""},
		{".autonomy subdir excluded by default → empty", ".autonomy/USER_GUIDANCE.md", ""},
		{".autonomy nested subdir excluded by default → empty", ".autonomy/foo/bar.md", ""},
		{"non-autonomy subdir returns its dir", "operator/notes.md", "operator"},
		{"deeply nested path returns full prefix", "operator/sub/notes.md", filepath.Join("operator", "sub")},
		{"whitespace trimmed then evaluated", "   operator/notes.md   ", "operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, projectCleanExcludeDir(tc.in))
		})
	}
}

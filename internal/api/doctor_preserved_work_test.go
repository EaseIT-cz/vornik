package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// audit 2026-10-12: a merge-back failure's work is pinned to
// refs/vornik/preserved/<task>/<attempt>-<ts> (GitHub #354, executor design
// §9.5.2 P3); the doctor lists those refs so an operator can find them. It
// reads ref files — a REST handler may not run git.
func TestCheckPreservedWork(t *testing.T) {
	t.Run("no workspaces root", func(t *testing.T) {
		h := &DoctorHandlers{}
		assert.Equal(t, "SKIPPED", h.checkPreservedWork().Status)
	})

	root := t.TempDir()
	clean := filepath.Join(root, "clean", ".git")
	require.NoError(t, os.MkdirAll(clean, 0o755))
	h := &DoctorHandlers{workspacesRoot: root}
	got := h.checkPreservedWork()
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "1 project")

	loose := filepath.Join(root, "proj-a", ".git", "refs", "vornik", "preserved", "task_a")
	require.NoError(t, os.MkdirAll(loose, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(loose, "2-1791840000"), []byte(strings.Repeat("a", 40)+"\n"), 0o644))
	packedDir := filepath.Join(root, "proj-b", ".git")
	require.NoError(t, os.MkdirAll(packedDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(packedDir, "packed-refs"),
		[]byte("# pack-refs with: peeled fully-peeled sorted\n"+strings.Repeat("b", 40)+" refs/vornik/preserved/task_b/1-1791840000\n"+
			strings.Repeat("c", 40)+" refs/heads/main\n"), 0o644))

	got = h.checkPreservedWork()
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "2 preserved")
	joined := strings.Join(got.Items, "\n")
	assert.Contains(t, joined, "proj-a: refs/vornik/preserved/task_a/2-1791840000 "+strings.Repeat("a", 40))
	assert.Contains(t, joined, "proj-b: refs/vornik/preserved/task_b/1-1791840000 "+strings.Repeat("b", 40))
	assert.NotContains(t, joined, "refs/heads/main")
}

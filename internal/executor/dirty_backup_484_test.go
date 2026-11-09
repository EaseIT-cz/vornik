package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #484 (executor design 06-executor.md §9.6.6, operator decision A10).
// Found at review (the #400 design's open decision O1); a user observes it at
// executor-e2e. The pre-work reset to origin discarded tracked uncommitted
// changes in the project clone with no backup. They are now kept in a
// refs/vornik/discarded/*-dirty-* ref before the reset.

func dirtyBackupRefs(t *testing.T, dir string) []string {
	t.Helper()
	out := gitOut(t, dir, "git", "for-each-ref", "--format=%(refname)", "refs/vornik/discarded/")
	var refs []string
	for _, r := range strings.Fields(out) {
		if strings.Contains(r, "-dirty-") {
			refs = append(refs, r)
		}
	}
	return refs
}

func TestPreWorkRefresh_DirtyTrackedChangeBackedUp_484(t *testing.T) {
	f := newRefreshFixture(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(f.projectDir, "base.txt"), []byte("dirty, unstaged\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(f.projectDir, "STAGED.md"), []byte("staged, never committed\n"), 0o644))
	mustGit(t, f.projectDir, "git", "add", "STAGED.md")

	saw, m := refreshRun(t, f, registry.ProjectWorkspace{RefreshBeforeWork: true, RefreshBranch: "main"},
		persistence.TaskCreationSourceScheduled)
	assert.True(t, saw, "the task still starts from origin")
	assert.Equal(t, 1.0, refreshCount(m, "scheduled", WorkspaceRefreshRefreshed))

	refs := dirtyBackupRefs(t, f.projectDir)
	require.Len(t, refs, 1, "the reset must keep the tracked uncommitted changes in one refs/vornik/discarded/*-dirty-* ref")
	assert.Equal(t, "dirty, unstaged", gitOut(t, f.projectDir, "git", "show", refs[0]+":base.txt"))
	assert.Equal(t, "staged, never committed", gitOut(t, f.projectDir, "git", "show", refs[0]+":STAGED.md"))
	body, err := os.ReadFile(filepath.Join(f.projectDir, "base.txt"))
	require.NoError(t, err)
	assert.Equal(t, "base\n", string(body), "the clone itself is reset")
}

func TestBackupDirtyTrackedChanges_CleanTreeWritesNoRef_484(t *testing.T) {
	requireGitExec(t)
	root := t.TempDir()
	_, scratch := s6Seed(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "UNTRACKED.md"), []byte("x\n"), 0o644))
	backupDirtyTrackedChanges(context.Background(), scratch, zerolog.Nop())
	assert.Empty(t, dirtyBackupRefs(t, scratch), "a tree with no tracked change (untracked files only) writes no ref")
	_, err := os.Stat(filepath.Join(scratch, "UNTRACKED.md"))
	assert.NoError(t, err, "an untracked file is left alone")
	assert.Empty(t, strings.TrimSpace(gitOut(t, scratch, "git", "stash", "list")), "refs/stash is not touched")
}

func TestCheckoutForgeChangeRequest_BacksUpDirtyTree_484(t *testing.T) {
	requireGitExec(t)
	root := t.TempDir()
	bare, scratch := s6Seed(t, root)
	mustGit(t, scratch, "git", "checkout", "-q", "-b", "pr")
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "pr.txt"), []byte("from the PR\n"), 0o644))
	mustGit(t, scratch, "git", "add", "-A")
	mustGit(t, scratch, "git", "commit", "-q", "-m", "pr")
	mustGit(t, scratch, "git", "push", "-q", "origin", "pr:refs/pull/7/head")
	clone := filepath.Join(root, "clone")
	mustGit(t, root, "git", "clone", "-q", bare, clone)
	require.NoError(t, os.WriteFile(filepath.Join(clone, "base.txt"), []byte("dirty\n"), 0o644))

	checkoutForgeChangeRequest(context.Background(), clone, "refs/pull/7/head", "main", zerolog.Nop())
	refs := dirtyBackupRefs(t, clone)
	require.Len(t, refs, 1, "the change-request reset must back up the dirty tree too")
	assert.Equal(t, "dirty", gitOut(t, clone, "git", "show", refs[0]+":base.txt"))
}

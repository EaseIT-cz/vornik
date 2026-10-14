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
)

// GitHub #220: a vornik-written workspace-root file on its FIRST write is
// untracked, `git add -u` cannot stage it, and workspaceDirty used to see it
// and refuse every merge in the project (a standing block, observed as an
// untracked BACKLOG.md on 2026-08-14). 2f1a32a37 fixed the prelude; these tests
// pin the outcome at the level that matters, mergeWorktree, and they iterate
// vornikInternalPaths so that every entry, including one added later, is
// covered by ONE definition of the set (design §4.4a, 2026-10-13 amendment).

// mergeFixture builds a project repo with one task worktree carrying a commit,
// ready for mergeWorktree.
func mergeFixture(t *testing.T, taskID string) (repo, wt string) {
	t.Helper()
	requireGitExec(t)
	repo = initGitRepoForTest(t)
	var err error
	wt, err = createWorktree(context.Background(), repo, taskID, zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "OUT.md"), []byte("work\n"), 0o644))
	return repo, wt
}

// writeInternal writes the vornik-internal entry p (a file name, or a
// directory entry ending in "/") into dir, as a first write.
func writeInternal(t *testing.T, dir, p string) {
	t.Helper()
	target := filepath.Join(dir, filepath.FromSlash(p))
	if strings.HasSuffix(p, "/") {
		require.NoError(t, os.MkdirAll(target, 0o755))
		target = filepath.Join(target, "scratch.md")
	}
	require.NoError(t, os.WriteFile(target, []byte("written by vornik\n"), 0o644))
}

// GitHub #220: every vornik-internal path, on its first write, must not block
// the merge.
func TestMergeWorktree_FirstWriteOfEachVornikInternalPathDoesNotBlock(t *testing.T) {
	for _, p := range vornikInternalPaths {
		t.Run(p, func(t *testing.T) {
			repo, wt := mergeFixture(t, "task_220_first_write")
			writeInternal(t, repo, p)

			require.NoError(t, mergeWorktree(context.Background(), repo, wt, "task_220_first_write",
				filepath.Join(repo, "BACKLOG.md"), nil, zerolog.Nop()),
				"a vornik-written %s on its first write must not block the merge", p)
			assert.Equal(t, "work", gitOut(t, repo, "git", "show", "main:OUT.md"))
		})
	}
}

// GitHub #220, the latent half: the set is defined once. A directory entry
// added to vornikInternalPaths must be honoured by workspaceDirty without a
// second edit there. Before 2026-10-13 workspaceDirty hardcoded `.autonomy`,
// so a new directory entry was neither staged (the prelude skips directories)
// nor filtered, and would have re-created the standing block.
func TestMergeWorktree_NewInternalDirectoryEntryIsHonouredByTheDirtyCheck(t *testing.T) {
	saved := vornikInternalPaths
	vornikInternalPaths = append(append([]string(nil), saved...), ".vornik-scratch/")
	t.Cleanup(func() { vornikInternalPaths = saved })

	repo, wt := mergeFixture(t, "task_220_new_dir")
	writeInternal(t, repo, ".vornik-scratch/")

	require.NoError(t, mergeWorktree(context.Background(), repo, wt, "task_220_new_dir", "", nil, zerolog.Nop()))
	assert.NotContains(t, gitOut(t, repo, "git", "ls-files"), ".vornik-scratch",
		"an internal directory is filtered, never committed")
}

// GitHub #220, the bound: a user-created untracked file still refuses the merge
// and is neither committed nor removed (§4.2: never absorb or destroy operator
// work).
func TestMergeWorktree_UserUntrackedFileStillBlocks(t *testing.T) {
	repo, wt := mergeFixture(t, "task_220_user_file")
	writeInternal(t, repo, "BACKLOG.md")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("mine\n"), 0o644))

	err := mergeWorktree(context.Background(), repo, wt, "task_220_user_file",
		filepath.Join(repo, "BACKLOG.md"), nil, zerolog.Nop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notes.txt")
	assert.NotContains(t, gitOut(t, repo, "git", "ls-files"), "notes.txt")
	_, statErr := os.Stat(filepath.Join(repo, "notes.txt"))
	assert.NoError(t, statErr, "the operator's file must survive the refusal")
}

// GitHub #220, forge shape: with the set in .git/info/exclude (what
// excludeVornikInternalPaths does on forge clones) the first write neither
// blocks nor gets committed.
func TestMergeWorktree_ForgeExcludedInternalFilesNeitherBlockNorCommit(t *testing.T) {
	repo, wt := mergeFixture(t, "task_220_forge")
	excludeVornikInternalPaths(repo, zerolog.Nop())
	for _, p := range vornikInternalPaths {
		writeInternal(t, repo, p)
	}

	require.NoError(t, mergeWorktree(context.Background(), repo, wt, "task_220_forge",
		filepath.Join(repo, "BACKLOG.md"), nil, zerolog.Nop()))
	tracked := gitOut(t, repo, "git", "ls-files")
	for _, p := range vornikInternalPaths {
		assert.NotContains(t, tracked, strings.TrimSuffix(p, "/"),
			"forge clone: %s must never be committed", p)
	}
}

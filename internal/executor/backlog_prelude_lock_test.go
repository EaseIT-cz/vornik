package executor

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// audit 2026-10-12 (w2-6): the merge prelude's backlog checkpoint (step B,
// "backlog: marker checkpoint before <task>") read BACKLOG.md without the
// backlog Store's per-project mutex. backlogfile.Store writes with
// os.WriteFile (truncate + rewrite), so a deposit landing during B's read
// could be committed half-written. Step C already takes the mutex
// (review-20261012-16a3 H1); B now takes the same one
// (workspace-state hygiene design §4.2b, "B under the Store mutex").

const tornBacklog = "# Backlog\n\n- [?] **[bug]** half a li"

// installTornWriter installs backlogPreludeReadHook as a model of a Store
// writer that arrives at B's read point. If mu is free, the writer takes it
// and leaves the file torn at that instant (as an os.WriteFile in progress
// would be). If mu is held — by B, the only possible holder here — the write
// is deferred, as a real writer would block on the mutex. Deterministic: no
// sleeps, TryLock from the single goroutine that could hold mu.
func installTornWriter(t *testing.T, mu *sync.Mutex, path string) (deferred *bool) {
	t.Helper()
	d := false
	backlogPreludeReadHook = func() {
		if mu.TryLock() {
			require.NoError(t, os.WriteFile(path, []byte(tornBacklog), 0o644))
			mu.Unlock()
			return
		}
		d = true
	}
	t.Cleanup(func() { backlogPreludeReadHook = nil })
	return &d
}

func TestAutoCommitTrackedChangesOnly_HoldsBacklogLockAcrossRead(t *testing.T) {
	dir := initGitRepoForTest(t)
	commitIn(t, dir, "BACKLOG.md", "# Backlog\n\n")
	bl := filepath.Join(dir, "BACKLOG.md")
	appendTo(t, bl, depositLine)

	var mu sync.Mutex
	deferred := installTornWriter(t, &mu, bl)
	lock := func(fn func()) { mu.Lock(); defer mu.Unlock(); fn() }

	autoCommitTrackedChangesOnly(context.Background(), dir, "task-B-lock", bl, lock, zerolog.Nop())

	assert.Equal(t, "backlog: marker checkpoint before task-B-lock", gitIn(t, dir, "log", "-1", "--format=%s"))
	committed := gitIn(t, dir, "show", "HEAD:BACKLOG.md")
	assert.NotContains(t, committed, "half a li", "B committed a torn BACKLOG.md: its read ran outside the Store mutex")
	assert.Contains(t, committed, "task_20260927075535_c5798f15038adcbc", "B commits the complete pre-write file")
	assert.True(t, *deferred, "the writer must have found the mutex held at B's read point")
}

// mergeWorktree threads its lock to B: the success-path merge holds the same
// mutex across the checkpoint.
func TestMergeWorktree_ThreadsBacklogLockToPrelude(t *testing.T) {
	dir := initGitRepoForTest(t)
	commitIn(t, dir, "BACKLOG.md", "# Backlog\n\n")
	bl := filepath.Join(dir, "BACKLOG.md")
	const taskID = "task_merge_lock"
	wt, err := createWorktree(context.Background(), dir, taskID, zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "OUT.md"), []byte("work\n"), 0o644))
	appendTo(t, bl, depositLine)

	var mu sync.Mutex
	deferred := installTornWriter(t, &mu, bl)
	calls := 0
	lock := func(fn func()) { calls++; mu.Lock(); defer mu.Unlock(); fn() }

	require.NoError(t, mergeWorktree(context.Background(), dir, wt, taskID, bl, lock, zerolog.Nop()))

	assert.Equal(t, 1, calls, "B runs once, under the lock")
	assert.True(t, *deferred)
	assert.NotContains(t, gitIn(t, dir, "show", "HEAD:BACKLOG.md"), "half a li")
}

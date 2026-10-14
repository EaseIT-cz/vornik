package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// audit 2026-10-12 (W3-9 open item, review 4a89 F4): a failed pin was visible
// only in the task error and an ERROR log. Every merge-back failure now counts
// once by what became of its work. Executor design §9.5.6.
func TestMergeBackPreservedMetric_ThreeOutcomesFromRealGit(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	m := NewMetrics(prometheus.NewRegistry())
	count := func(r string) float64 {
		return testutil.ToFloat64(m.MergeBackPreservedTotal.WithLabelValues("proj", r))
	}

	// pinned: a real conflict.
	repo, wt := conflictingWorktree(t, "task_metric_pin")
	require.Error(t, mergeWorktree(ctx, repo, wt, "task_metric_pin", "", nil, zerolog.Nop()))
	pw, err := preserveFailedWork(ctx, repo, wt, "task_metric_pin", 1, time.Now(), zerolog.Nop())
	m.RecordMergeBackPreserved("proj", pw, err)
	assert.Equal(t, 1.0, count(MergeBackPreservedPinned))

	// nothing_to_pin: the worktree HEAD is already on the project's HEAD.
	repo2 := seedRepo(t, t.TempDir(), "proj")
	wt2, err := createWorktree(ctx, repo2, "task_metric_none", zerolog.Nop())
	require.NoError(t, err)
	pw, err = preserveFailedWork(ctx, repo2, wt2, "task_metric_none", 1, time.Now(), zerolog.Nop())
	m.RecordMergeBackPreserved("proj", pw, err)
	assert.Equal(t, 1.0, count(MergeBackPreservedNothingToPin))

	// pin_failed: the ref store cannot be written.
	repo3, wt3 := conflictingWorktree(t, "task_metric_fail")
	require.Error(t, mergeWorktree(ctx, repo3, wt3, "task_metric_fail", "", nil, zerolog.Nop()))
	refs := filepath.Join(repo3, ".git", "refs", "vornik")
	require.NoError(t, os.WriteFile(refs, []byte("not a directory"), 0o644))
	pw, err = preserveFailedWork(ctx, repo3, wt3, "task_metric_fail", 1, time.Now(), zerolog.Nop())
	require.Error(t, err)
	m.RecordMergeBackPreserved("proj", pw, err)
	assert.Equal(t, 1.0, count(MergeBackPreservedPinFailed))

	// nil metrics is safe.
	var nilM *Metrics
	assert.NotPanics(t, func() { nilM.RecordMergeBackPreserved("proj", PreservedWork{}, nil) })
}

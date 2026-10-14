package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: MERGE_FAILED was assigned by text match (GitHub #353).
// 25 of the 34 MERGE_FAILED executions in 30 days on prod were agent
// containers that exited 1 with no reason from the agent; handleFailure passed
// the full message (400-line log tail included) as the classifier's hint, and
// the tail said "worktree" and "fail". Permanent-failure design §9.

// prodCrashLogTail is the shape of the prod log tails (exec_20260918001245_…,
// exec_20261002225858_…), plus the words the old switch matched on.
func prodCrashLogTail() string {
	return containerLogSection(strings.Join([]string{
		"[vornik-agent] starting (model=glm-5.2)",
		"jq: error: Could not open file /app/input/task.json: Permission denied",
		"[vornik-agent] FATAL: contract mount unusable: cannot read /app/input/task.json (running as uid:gid 1000:1000)",
		"[vornik-agent] git merge of /app/workspace/project failed: could not read .worktrees/task_x/HEAD",
		"[vornik-agent] ERROR: unexpected exit (code 78), writing emergency result",
		"/entrypoint.sh: line 4179: /app/output/result.json: Permission denied",
	}, "\n"))
}

// classifyAsHandleFailure classifies err exactly as handleFailure does: the
// error, and its full message again as the hint.
func classifyAsHandleFailure(err error) string {
	return ClassifyExecutionFailure(err, err.Error())
}

func TestMergeFailedClass_ContainerCrashLogTailIsNotAMergeFailure(t *testing.T) {
	crash := newContainerCrashError(1, "container exited with code 1"+prodCrashLogTail())
	for name, err := range map[string]error{
		"lead planning":       fmt.Errorf("lead planning failed: %w", crash),
		"terminal (on_fail)":  fmt.Errorf("Architectural review failed (last step: review, reason: %s): %w", crash.Error()[:400], crash),
		"bare":                crash,
		"plain text, no type": errors.New("lead planning failed: container exited with code 1" + prodCrashLogTail()),
	} {
		t.Run(name, func(t *testing.T) {
			got := classifyAsHandleFailure(err)
			assert.NotEqual(t, persistence.TaskFailureClassMergeFailed, got,
				"a container log tail mentioning merge/worktree must not classify as MERGE_FAILED")
		})
	}
}

func TestMergeFailedClass_ContainerCrashIsRuntimeError(t *testing.T) {
	crash := newContainerCrashError(1, "container exited with code 1"+prodCrashLogTail())
	for name, err := range map[string]error{
		"lead planning":      fmt.Errorf("lead planning failed: %w", crash),
		"terminal (on_fail)": fmt.Errorf("Adaptive routing failed (last step: route, reason: %s): %w", crash.Error()[:300], crash),
		"bare":               crash,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, persistence.TaskFailureClassRuntimeError, classifyAsHandleFailure(err))
			assert.Equal(t, persistence.TaskFailureClassRuntimeError, ClassifyExecutionFailure(err, ""))
		})
	}
}

// A container that exited non-zero AFTER the agent said why keeps the class
// its reason names: only a crash with no reason is RUNTIME_ERROR.
func TestMergeFailedClass_NonCrashContainerExitKeepsItsTextClass(t *testing.T) {
	err := newContainerExitError(1, `schema violation: role "analyst" result.json is missing required keys: [analysis:object]`+prodCrashLogTail())
	assert.Equal(t, persistence.TaskFailureClassInvalidOutput, classifyAsHandleFailure(err))
}

// Free text that describes a merge failure, with no typed error behind it, is
// no longer MERGE_FAILED: the class is raised by the merge-back, not read.
func TestMergeFailedClass_FreeTextIsNotEnough(t *testing.T) {
	for _, msg := range []string{
		"could not merge worktree onto main",
		"agent steps succeeded but changes could not be merged to master: git merge of worktree branch worktree/t failed",
	} {
		assert.NotEqual(t, persistence.TaskFailureClassMergeFailed, ClassifyExecutionFailure(nil, msg), msg)
		assert.NotEqual(t, persistence.TaskFailureClassMergeFailed, classifyAsHandleFailure(errors.New(msg)), msg)
	}
}

// A real merge conflict, on real git, wrapped as the merge-back wraps it, is
// MERGE_FAILED — with and without the hint.
func TestMergeFailedClass_RealMergeConflictIsMergeFailed(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	repo := seedRepo(t, t.TempDir(), "proj")
	taskID := "task_w3_9_conflict"
	wt, err := createWorktree(ctx, repo, taskID, zerolog.Nop())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "X"), []byte("operator\n"), 0o644))
	mustGit(t, repo, "git", "add", "-A")
	mustGit(t, repo, "git", "commit", "-q", "-m", "operator push")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "X"), []byte("agent\n"), 0o644))

	mergeErr := mergeWorktree(ctx, repo, wt, taskID, "", nil, zerolog.Nop())
	require.Error(t, mergeErr, "modify/modify conflict must fail the merge")

	err = fmt.Errorf("agent steps succeeded but changes could not be merged to master: %w", &mergeBackError{err: mergeErr})
	assert.Equal(t, persistence.TaskFailureClassMergeFailed, ClassifyExecutionFailure(err, ""))
	assert.Equal(t, persistence.TaskFailureClassMergeFailed, classifyAsHandleFailure(err))
}

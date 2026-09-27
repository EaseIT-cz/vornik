package executor

import (
	"context"
	"errors"
	"os"
	"testing"

	"vornik.io/vornik/internal/spawn"
)

// The process-spawn law's GitWorkspace kind refuses a directory outside a
// registered workspace root (design "S1b-2, as built"); the daemon registers
// runtime.project_workspace_path. These tests run git in t.TempDir()
// repositories, so the temp root is theirs.
func init() {
	if err := spawn.RegisterWorkspaceRoot(os.TempDir()); err != nil {
		panic(err)
	}
}

// The production runner is the spawn law's GitWorkspace: a directory outside
// the registered root, a call without the leading -C, or an option that names a
// program is refused before git starts.
func TestExecGitRunner_RefusesOutsideTheWorkspaceShape(t *testing.T) {
	ctx := context.Background()
	r := execGitRunner{}
	for _, args := range [][]string{
		{"-C", "/etc", "status"},
		{"status"},
		{"-C", t.TempDir(), "fetch", "--upload-pack=touch /tmp/x", "origin"},
		{"-C", t.TempDir(), "-c", "core.hooksPath=/tmp", "status"},
	} {
		if _, err := r.combined(ctx, args...); !errors.Is(err, spawn.ErrRefused) {
			t.Errorf("combined %q: want spawn.ErrRefused, got %v", args, err)
		}
		if _, err := r.output(ctx, args...); !errors.Is(err, spawn.ErrRefused) {
			t.Errorf("output %q: want spawn.ErrRefused, got %v", args, err)
		}
	}
	if worktreeInUseByContainer(ctx, "") {
		t.Error("an empty worktree dir is never in use")
	}
}

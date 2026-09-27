package executor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/spawn"
)

// s6ComposeDaemonGitConfig composes the daemon's git config from the
// operator's HOME and registers it, exactly as the daemon's startup does
// (process-spawn law S6-D4), restoring the previous registration afterwards.
// The host's system config is kept out of the test.
func s6ComposeDaemonGitConfig(t *testing.T, home string) {
	t.Helper()
	_ = home // HOME/XDG_CONFIG_HOME are already pointed at it
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	restore := spawn.RegisterGitGlobalConfig(spawn.GitGlobalConfig())
	t.Cleanup(restore)
	if _, err := spawn.ComposeGitConfig(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("compose the daemon's git config: %v", err)
	}
}

// s6FlowRefusalsWithPartialDirs: on the real worktree, naming only one of the
// two git dirs, or the wrong work tree, is refused too, and runs nothing
// (S6-D3).
func s6FlowRefusalsWithPartialDirs(t *testing.T, wt, marker string) {
	t.Helper()
	root := projectRootFromWorktree(wt)
	admin := worktreeAdminDir(root, "task_s6_d3flow")
	for name, dirs := range map[string]spawn.GitDirs{
		"GIT_DIR only":        {GitDir: admin},
		"GIT_WORK_TREE only":  {WorkTree: wt},
		"wrong GIT_WORK_TREE": {GitDir: admin, WorkTree: root},
	} {
		cmd, err := spawn.GitWorkspaceDirs(context.Background(), wt, dirs, "status", "--porcelain")
		if err == nil {
			_, _ = cmd.CombinedOutput()
		}
		if !errors.Is(err, spawn.ErrRefused) {
			t.Errorf("%s: want spawn.ErrRefused, got %v", name, err)
		}
	}
	s6AssertAbsent(t, marker, "a refused partial-pin command still ran git")
	// And the daemon's own path, which derives both, runs and reads the truth.
	if _, err := gitExec.output(context.Background(), "-C", wt, "status", "--porcelain"); err != nil {
		t.Errorf("the executor's git runner must run a pinned worktree command: %v", err)
	}
	s6AssertAbsent(t, marker, "the pinned command followed the tampered pointer")
}

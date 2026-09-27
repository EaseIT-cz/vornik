package executor

import (
	"context"
	"fmt"
	"path/filepath"

	"vornik.io/vornik/internal/spawn"
)

// gitRunner abstracts running git subcommands so the executor's git-backed
// helpers (gitHEAD, resetWorkspace, cleanProjectDir, claim verification, and
// — converted separately — worktree management) become unit-testable without
// a real repository on disk.
//
// Production uses execGitRunner via the package-level `gitExec`; tests swap
// gitExec for a fake (see withGitRunner in the test file). The default is
// behaviour-preserving: it shells out exactly as the previous inline
// exec.CommandContext(ctx, "git", ...) calls did, including the .Output()
// vs .CombinedOutput() distinction the call sites relied on.
//
// This is the P2 code-quality seam (2026-06-19). It is introduced as a
// package var rather than an Executor field because the git helpers are
// package functions, not methods — routing through the var converts them
// with zero call-site churn. The high-churn worktree.go sites are migrated
// onto this same seam in a follow-up.
type gitRunner interface {
	// output runs `git <args...>` and returns stdout (mirrors exec.Cmd.Output()).
	output(ctx context.Context, args ...string) ([]byte, error)
	// combined runs `git <args...>` and returns stdout+stderr
	// (mirrors exec.Cmd.CombinedOutput()); also used where the caller only
	// cared about the exit status (the former .Run() sites).
	combined(ctx context.Context, args ...string) ([]byte, error)
}

// execGitRunner runs git through the process-spawn law's GitWorkspace kind
// (internal/spawn, design "S1b-2, as built"): every call site passes
// `-C <dir>` first, and spawn refuses a dir outside the registered workspace
// root, a subcommand outside the orchestration set, and any option that names
// a program. A refusal returns before any process starts.
type execGitRunner struct{}

func (execGitRunner) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd, err := gitWorkspaceCmd(ctx, args)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

func (execGitRunner) combined(ctx context.Context, args ...string) ([]byte, error) {
	cmd, err := gitWorkspaceCmd(ctx, args)
	if err != nil {
		return nil, err
	}
	return cmd.CombinedOutput()
}

// gitWorkspaceCmd splits the `-C <dir>` every executor git call leads with.
// When dir is a task worktree (<project>/.worktrees/<task>) the command names
// the worktree's git dir itself, derived from the daemon's own paths and never
// from the worktree's agent-writable .git file (process-spawn law S6-D2); spawn
// refuses a worktree command without it (S6-D3).
func gitWorkspaceCmd(ctx context.Context, args []string) (*spawn.Cmd, error) {
	if len(args) < 2 || args[0] != "-C" {
		return nil, fmt.Errorf("%w: executor git call without a leading -C <dir>: %q", spawn.ErrRefused, args)
	}
	if dirs, ok := worktreeGitDirs(args[1]); ok {
		return spawn.GitWorkspaceDirs(ctx, args[1], dirs, args[2:]...)
	}
	return spawn.GitWorkspace(ctx, args[1], args[2:]...)
}

// worktreeGitDirs is the daemon's own answer to "which repository does this
// task worktree belong to": <project>/.git/worktrees/<task> for
// <project>/.worktrees/<task>, from the path layout worktreePath creates.
func worktreeGitDirs(dir string) (spawn.GitDirs, bool) {
	root := projectRootFromWorktree(dir)
	if root == "" {
		return spawn.GitDirs{}, false
	}
	return spawn.GitDirs{GitDir: worktreeAdminDir(root, filepath.Base(dir)), WorkTree: dir}, true
}

// gitExec is the package-level git runner. Production leaves it as the real
// exec-backed implementation; tests swap it via withGitRunner.
var gitExec gitRunner = execGitRunner{}

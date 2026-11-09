package autonomy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"

	"vornik.io/vornik/internal/gitbackup"
	"vornik.io/vornik/internal/spawn"
)

// ExecGitRefresh is the default gitRefresh hook (wired via WithGitRefresh):
// fetch origin and hard-reset repoDir to origin/<current-branch>, so a backlog
// tick reads the workspace at the tip of main including external contributions
// merged since the last iteration.
//
// It only touches a real git checkout — a non-repo repoDir is a clean no-op so
// a mis-provisioned workspace never errors the tick. The hard reset discards
// stale local worktree-merge commits and any non-BACKLOG.md drift; the
// caller's RefreshFromOrigin runs this inside the backlog lock and re-applies
// the local consumption marks afterwards, so no [x]/[!] state is lost.
//
// Before the reset it keeps local-only commits and tracked uncommitted
// changes under refs/vornik/discarded/, the same rule as the executor's
// task-start resets (internal/gitbackup; executor design §9.6.7, GitHub #675).
// backlogRel is the backlog file's path relative to repoDir: its marks alone
// are no reason to write a backup ref, since they are re-applied every tick.
func ExecGitRefresh(ctx context.Context, repoDir, backlogRel string) error {
	if fi, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil || (!fi.IsDir() && fi.Size() == 0) {
		return nil // not a git checkout — nothing to refresh
	}
	run := func(args ...string) (string, error) {
		// The process-spawn law's GitWorkspace kind (internal/spawn): repoDir
		// must lie under the registered workspace root.
		cmd, err := spawn.GitWorkspace(ctx, repoDir, args...)
		if err != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := run("fetch", "--quiet", "origin"); err != nil {
		return err
	}
	branch, err := run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" || branch == "HEAD" {
		branch = "main" // detached / unknown → assume the default branch
	}
	backupRun := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd, err := spawn.GitWorkspace(ctx, repoDir, args...)
		if err != nil {
			return nil, err
		}
		return cmd.Output()
	}
	logger := log.Ctx(ctx).With().Str("repo_dir", repoDir).Logger()
	gitbackup.LocalOnlyCommits(ctx, backupRun, branch, logger)
	ignore := ""
	if backlogRel != "" {
		ignore = filepath.ToSlash(filepath.Clean(backlogRel))
	}
	gitbackup.DirtyTrackedChanges(ctx, backupRun, ignore, logger)
	_, err = run("reset", "--hard", "origin/"+branch)
	return err
}

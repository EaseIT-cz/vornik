package hostdoctor

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitRun runs git; a seam so tests never shell out.
var gitRun = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", args...).CombinedOutput()
}

// CleanWorktreeGit cleans git's administrative side of the orphan worktrees
// the daemon's orphan_worktrees fix removed, each named "<project>/<taskID>":
// `git worktree prune` so `git worktree list` stops reporting the entry, and
// `git branch -D worktree/<taskID>` so a later `git worktree add` for the same
// task does not fail with "already exists".
//
// The daemon did this itself until the process-spawn law, S2: it removes the
// directories (no spawn) and reports them, and vornikctl doctor --fix runs git
// here, on the host. Best-effort like before — a project that is not a git
// repository makes both commands fail harmlessly; a failure is reported, not
// fatal. Entries that do not name a single project and task are skipped.
func CleanWorktreeGit(ctx context.Context, workspacesRoot string, removed []string) []string {
	var notes []string
	if workspacesRoot == "" {
		return []string{"git cleanup skipped: no runtime.project_workspace_path in the config"}
	}
	root := filepath.Clean(workspacesRoot)
	for _, r := range removed {
		// The list is produced by the daemon and consumed here, where git runs
		// on it, so every entry is validated rather than trusted: each half must
		// be one plain path component, and the joined directory must stay under
		// the workspaces root (S2 code review, review-20260926-f198 #3).
		project, taskID, ok := strings.Cut(r, "/")
		projectDir := filepath.Join(root, project)
		if !ok || !plainComponent(project) || !plainComponent(taskID) ||
			filepath.Dir(projectDir) != root {
			notes = append(notes, fmt.Sprintf("%q: not a <project>/<task> entry, skipped", r))
			continue
		}
		if out, err := gitRun(ctx, "-C", projectDir, "worktree", "prune"); err != nil {
			notes = append(notes, fmt.Sprintf("%s: git worktree prune: %v %s", r, err, strings.TrimSpace(string(out))))
		}
		// `--` separator: the branch argument is never read as a flag.
		if out, err := gitRun(ctx, "-C", projectDir, "branch", "-D", "--", "worktree/"+taskID); err != nil {
			notes = append(notes, fmt.Sprintf("%s: git branch -D: %v %s", r, err, strings.TrimSpace(string(out))))
		}
	}
	return notes
}

// plainComponent reports whether s is a single, ordinary path component: not
// empty or blank, not "." or "..", and free of separators and control
// characters (a NUL or newline has no business in a project or task id).
func plainComponent(s string) bool {
	if strings.TrimSpace(s) == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

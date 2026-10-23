package executor

import (
	"context"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// workspaceRefreshTimeout bounds the opt-in pre-work refresh. It runs inside
// the project's workspace lock, so a hung fetch must not hold every other task
// of the project; 90 seconds is the autonomy backlog tick's value for the same
// fetch (executor design §9.6.2).
const workspaceRefreshTimeout = 90 * time.Second

// refreshSource returns the metric's source label for a task the opt-in
// refresh applies to, and ok=false for every other creation source. Only
// unattended work is refreshed: a person or a parent decides the starting
// point of every other task, and a child must start from its parent's tree
// (design §9.6.2, open decision O2).
func refreshSource(s persistence.TaskCreationSource) (string, bool) {
	switch s {
	case persistence.TaskCreationSourceScheduled:
		return "scheduled", true
	case persistence.TaskCreationSourceAutonomous:
		return "autonomous", true
	}
	return "", false
}

// resolveRefreshBranch is the configured branch, else the clone's current
// branch; "" when HEAD is detached or unreadable (never guessed).
// `rev-parse --abbrev-ref HEAD` prints "HEAD" when detached; symbolic-ref is
// outside the process-spawn law's git subcommand set.
func resolveRefreshBranch(ctx context.Context, projectDir, configured string) string {
	if b := strings.TrimSpace(configured); b != "" {
		return b
	}
	out, err := gitExec.output(ctx, "-C", projectDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	b := strings.TrimSpace(string(out))
	if b == "HEAD" || strings.HasPrefix(b, "-") {
		return ""
	}
	return b
}

// refreshBeforeWork is the opt-in pre-work refresh for non-forge tasks
// (workspace.refresh_before_work; executor design 06-executor.md §9.6, GitHub
// #400). The caller holds the project's workspace lock and has already
// excluded forge tasks. It applies the forge path's own reset
// (rebaseProjectToOrigin) to a SCHEDULED or AUTONOMOUS task, and counts every
// such task exactly once, refreshed or not, so a failure is never silent and
// the sum over outcomes is the number of eligible tasks.
func (e *Executor) refreshBeforeWork(ctx context.Context, task *persistence.Task, project *registry.Project, projectDir string, adoptingKeptWorktree bool) {
	if project == nil || !project.Workspace.RefreshBeforeWork || projectDir == "" {
		return
	}
	source, eligible := refreshSource(task.CreationSource)
	if !eligible {
		e.logger.Debug().Str("task_id", task.ID).Str("project", task.ProjectID).
			Str("creation_source", string(task.CreationSource)).
			Msg("pre-work refresh: not applied — only scheduled and autonomous tasks are refreshed")
		return
	}

	branch := ""
	outcome := ""
	switch {
	case project.NormalizedAutonomyMode() == registry.AutonomyModeBacklog:
		// The validator refuses this combination; this is the defence for a
		// project loaded without it. The backlog tick refreshes in its own
		// lock and re-applies consumption marks a reset here could drop.
		outcome = WorkspaceRefreshBacklogMode
	case adoptingKeptWorktree:
		outcome = WorkspaceRefreshResume
	case e.taskHasChildren(ctx, task.ID):
		outcome = WorkspaceRefreshHasChildren
	default:
		branch = resolveRefreshBranch(ctx, projectDir, project.Workspace.RefreshBranch)
		if branch == "" {
			outcome = WorkspaceRefreshNoBranch
			break
		}
		rctx, cancel := context.WithTimeout(ctx, workspaceRefreshTimeout)
		outcome = rebaseProjectToOrigin(rctx, projectDir, branch, e.logger)
		cancel()
	}

	e.metrics.RecordWorkspaceRefresh(task.ProjectID, source, outcome)
	ev := e.logger.Warn()
	msg := "pre-work refresh: NOT refreshed — the task runs on the clone's existing HEAD"
	if outcome == WorkspaceRefreshRefreshed {
		ev = e.logger.Info()
		msg = "pre-work refresh: project clone reset to origin before the task's worktree"
	}
	ev.Str("task_id", task.ID).Str("project", task.ProjectID).Str("source", source).
		Str("branch", branch).Str("outcome", outcome).Msg(msg)
}

// taskHasChildren reports whether the task already has children (a delegator
// resuming after its children merged into the clone). A lookup error reads as
// no children, the forge path's rule.
func (e *Executor) taskHasChildren(ctx context.Context, taskID string) bool {
	if e.taskRepo == nil {
		return false
	}
	children, err := e.taskRepo.GetChildren(ctx, taskID)
	return err == nil && len(children) > 0
}

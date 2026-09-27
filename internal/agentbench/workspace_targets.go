package agentbench

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// Every task starts from a pristine workspace (benchmark LLD §12.23).

// WorkspaceReset values recorded in RunManifest.WorkspaceReset.
const (
	// WorkspaceResetTargets: a workspace was given and each task repeat's
	// declared targets were cleared before it ran (possibly none, for "[]").
	WorkspaceResetTargets = "targets"
	// WorkspaceResetNoWorkspace: no workspace was given, so nothing was
	// cleared and the run is not fresh-workspace evidence.
	WorkspaceResetNoWorkspace = "none-no-workspace"
)

// WorkspacePreparer clears a task's targets before it runs and reports which
// it produced afterwards. The git implementation lives in internal/cli:
// only vornikctl spawns processes.
type WorkspacePreparer interface {
	// Prepare removes spec's targets from the workspace HEAD and returns the
	// commit that did it ("" when nothing needed removing).
	Prepare(ctx context.Context, spec TaskSpec, repeat int) (commit string, err error)
	// Produced reports, per target, whether it exists at the workspace HEAD.
	// Called once the daemon reports the task terminal; the executor merges a
	// task's worktree BEFORE marking it completed (executor.go:
	// cleanupWorktree(true) precedes handleSuccess), so HEAD is post-merge.
	Produced(ctx context.Context, spec TaskSpec) (map[string]bool, error)
}

// ValidateTaskTargets checks every task's targets: relative, clean, and named
// verbatim in the task's prompt. With requireKey (a workspace was given),
// every task must carry the key; one task without it refuses the set.
func ValidateTaskTargets(tasks []TaskSpec, requireKey bool) error {
	for _, task := range tasks {
		// JSON gives an absent key as nil and "[]" as an empty non-nil
		// slice, which is the distinction this needs; validate at LOAD, on
		// the decoded set.
		if requireKey && task.Targets == nil {
			return fmt.Errorf("task %q declares no \"targets\": with a workspace every task must list the paths it creates "+
				"(or [] for none), or it would run against earlier runs' output (benchmark LLD §12.23)", task.ID)
		}
		// A suite is copied into one package, so a graded task names exactly
		// one target; more would be graded partially and silently (§12.24).
		if task.Acceptance != "" && len(task.Targets) != 1 {
			return fmt.Errorf("task %q has an acceptance suite, so it must declare exactly one target (it declares %d)",
				task.ID, len(task.Targets))
		}
		for _, tg := range task.Targets {
			clean := path.Clean(tg)
			if tg == "" || path.IsAbs(tg) || clean == ".." || strings.HasPrefix(clean, "../") {
				return fmt.Errorf("task %q target %q must be a relative path inside the workspace", task.ID, tg)
			}
			if !strings.Contains(task.Prompt, tg) {
				return fmt.Errorf("task %q target %q does not appear in its prompt; targets must be what the prompt asks for", task.ID, tg)
			}
		}
	}
	return nil
}

// TargetsMissing counts the declared targets the task did not produce.
func (r TaskRun) TargetsMissing() int {
	n := 0
	for _, ok := range r.TargetsProduced {
		if !ok {
			n++
		}
	}
	return n
}

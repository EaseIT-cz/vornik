package agentbench

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
)

// Every task starts from a pristine workspace (benchmark LLD §12.23).

// WorkspaceReset values recorded in RunManifest.WorkspaceReset.
const (
	// WorkspaceResetTargets: a workspace was given and each task repeat's
	// declared targets were cleared before it ran (possibly none, for "[]").
	WorkspaceResetTargets = "targets"
	// WorkspaceResetTargetsAndState: as WorkspaceResetTargets, and the
	// workflow's own state (WorkflowStatePaths) was cleared too, so no task
	// could resume an earlier task's feature (§12.29, harness 10).
	WorkspaceResetTargetsAndState = "targets+workflow-state"
	// WorkspaceResetTargetsStateAndHistory: as WorkspaceResetTargetsAndState,
	// and the history was replaced by one root commit after archiving it, so
	// no task could read an earlier task's work with git (§12.30, harness 11).
	// Earlier tasks' committed files still carried over (§12.33).
	WorkspaceResetTargetsStateAndHistory = "targets+workflow-state+history"
	// WorkspaceResetBaseline: each repeat started from the harness's
	// baseline files plus its seed, with one root commit (§12.33, harness 12).
	WorkspaceResetBaseline = "baseline+seed+history"
	// WorkspaceResetNoWorkspace: no workspace was given, so nothing was
	// cleared and the run is not fresh-workspace evidence.
	WorkspaceResetNoWorkspace = "none-no-workspace"
)

// BaselineGoMod is the workspace's root module. The dev-swarm tasks with a
// scratch/ target build their packages under it; no harness code wrote it
// before harness 12, so they depended on one an early agent left (§12.33).
const BaselineGoMod = "module scratch\n\ngo 1.21\n"

// BaselineGoModPath and BaselineMarkerPath are the two files every repeat
// starts from, besides its seed (benchmark LLD §12.33). The harness writes
// both on every reset; every other tracked path is removed.
const (
	BaselineGoModPath  = "go.mod"
	BaselineMarkerPath = ".vornik/bootstrap.md"
)

// underBaseline reports whether a target is, or lies under, a baseline path.
func underBaseline(target string) bool {
	clean := path.Clean(target)
	for _, b := range []string{BaselineGoModPath, path.Dir(BaselineMarkerPath)} {
		if clean == b || strings.HasPrefix(clean, b+"/") {
			return true
		}
	}
	return false
}

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
		// A seed is copied into one target, the same rule (GitHub #650).
		if task.Seed != "" && len(task.Targets) != 1 {
			return fmt.Errorf("task %q has a seed, so it must declare exactly one target (it declares %d)",
				task.ID, len(task.Targets))
		}
		if task.Seed != "" && task.attachmentBase != "" {
			dir, err := task.SeedDir()
			if err != nil {
				return err
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				return fmt.Errorf("task %q: its seed %q is not a directory beside the task set", task.ID, task.Seed)
			}
		}
		for _, tg := range task.Targets {
			clean := path.Clean(tg)
			if tg == "" || path.IsAbs(tg) || clean == ".." || strings.HasPrefix(clean, "../") {
				return fmt.Errorf("task %q target %q must be a relative path inside the workspace", task.ID, tg)
			}
			// A target under a baseline path would be overwritten by every
			// reset (§12.33).
			if underBaseline(clean) {
				return fmt.Errorf("task %q target %q lies under the workspace baseline (%s, %s), which every reset rewrites",
					task.ID, tg, BaselineGoModPath, path.Dir(BaselineMarkerPath))
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

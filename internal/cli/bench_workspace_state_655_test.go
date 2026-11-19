package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

// The workflow's state is reset with the targets (benchmark LLD §12.29,
// GitHub #655).
//
// Incident, found at production (operator observation of the 2026-11-06
// recalibration): lh-06 r2 found lh-02's scratch/lhworker marked in progress
// in the workspace's BACKLOG.md, resumed that feature instead of its own, and
// was journaled as succeeded without writing scratch/lhpipe. §12.23 cleared
// only declared targets, so BACKLOG.md and .autonomy/ carried every earlier
// task's work forward.
//
// Lower because: the reset is gitWorkspace.Prepare, exercised here on a real
// git repository exactly as vornikctl runs it before each bench task; the
// bench re-run that follows the fix is the e2e check.

func TestGitWorkspace_PrepareResetsWorkflowStateWithTheTargets_655(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, "BACKLOG.md", "- [~] **[feature]** scratch/lhworker: resume me\n")
	commitFile(t, dir, ".autonomy/CURRENT_TASK.md", "# CURRENT TASK — scratch/lhworker\n- [ ] Subtask 2\n")
	commitFile(t, dir, ".autonomy/review-notes.md", "notes\n")
	commitFile(t, dir, ".vornik/bootstrap.md", "bootstrap\n")
	commitFile(t, dir, "scratch/lhworker/backoff.go", "package lhworker\n")
	ws := gitWorkspace{dir: dir}

	// lh-06's own target is absent: the state must be reset anyway.
	spec := agentbench.TaskSpec{ID: "lh-06", Targets: []string{"scratch/lhpipe"}}
	commit, err := ws.Prepare(context.Background(), spec, 2)
	if err != nil || commit == "" {
		t.Fatalf("tracked workflow state is removed in a commit even with no target present: %q %v", commit, err)
	}
	for _, gone := range []string{"BACKLOG.md", ".autonomy"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s must be gone before the task runs", gone)
		}
	}
	// Since §12.33 (#808) an earlier task's file is removed too: only the
	// baseline carries over. This assertion used to say it "must be left
	// alone", which was the leak.
	if _, err := os.Stat(filepath.Join(dir, ".vornik/bootstrap.md")); err != nil {
		t.Error("the baseline marker must stay")
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch/lhworker/backoff.go")); !os.IsNotExist(err) {
		t.Error("an earlier task's file must be gone before the task runs (§12.33)")
	}
	msg, _ := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	if !strings.Contains(string(msg), "lh-06 r2") || !strings.Contains(string(msg), "§12.29") {
		t.Fatalf("the reset commit names the task, the repeat and the rule: %q", msg)
	}
	if status, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); len(status) != 0 {
		t.Fatalf("the reset leaves a clean workspace: %q", status)
	}

	// Nothing left to reset: no empty commit.
	again, err := ws.Prepare(context.Background(), spec, 3)
	if err != nil || again != "" {
		t.Fatalf("with no target and no state tracked, Prepare is a no-op: %q %v", again, err)
	}
}

// The baseline is the two files the harness writes (§12.33, #808); it
// replaced WorkflowStatePaths, which only named what to remove.
func TestBaselineIsTheHarnessFiles_808(t *testing.T) {
	got := baselineFiles()
	if len(got) != 2 || got["go.mod"] != agentbench.BaselineGoMod || got[".vornik/bootstrap.md"] == "" {
		t.Fatalf("baseline = %v", got)
	}
}

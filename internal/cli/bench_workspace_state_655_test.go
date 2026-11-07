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
	for _, kept := range []string{".vornik/bootstrap.md", "scratch/lhworker/backoff.go"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s is neither a target nor workflow state and must be left alone", kept)
		}
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

func TestWorkflowStatePathsAreTheDevPipelineState_655(t *testing.T) {
	want := map[string]bool{".autonomy": true, "BACKLOG.md": true}
	if len(agentbench.WorkflowStatePaths) != len(want) {
		t.Fatalf("WorkflowStatePaths = %v", agentbench.WorkflowStatePaths)
	}
	for _, p := range agentbench.WorkflowStatePaths {
		if !want[p] {
			t.Fatalf("unexpected workflow state path %q", p)
		}
	}
}

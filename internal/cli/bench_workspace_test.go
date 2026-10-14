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

// The benchmark workspace is cleared of a task's targets before each repeat
// (benchmark LLD §12.23). Incident: the slow-hardware arms' tasks found their
// "NEW" files already written by arms from 2026-08-14.

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "master"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func commitFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", rel}, {"commit", "-q", "-m", "add " + rel}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func TestGitWorkspace_ClearsExistingTargetsAndCommits(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, "scratch/registry/registry.go", "package registry\n")
	commitFile(t, dir, "scratch/other/other.go", "package other\n")
	ws := gitWorkspace{dir: dir}
	spec := agentbench.TaskSpec{ID: "dp-01", Targets: []string{"scratch/registry"}}
	commit, err := ws.Prepare(context.Background(), spec, 1)
	if err != nil || commit == "" {
		t.Fatalf("an existing target is removed in a commit: %q %v", commit, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch/registry")); !os.IsNotExist(err) {
		t.Fatal("the target must be gone from the working tree")
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch/other/other.go")); err != nil {
		t.Fatal("anything that is not a target must be left alone")
	}
	msg, _ := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	if !strings.Contains(string(msg), "dp-01") || !strings.Contains(string(msg), "§12.23") {
		t.Fatalf("the clear commit names the task and the rule: %q", msg)
	}
	produced, err := ws.Produced(context.Background(), spec)
	if err != nil || produced["scratch/registry"] {
		t.Fatalf("after clearing, the target is not produced: %v %v", produced, err)
	}
	commitFile(t, dir, "scratch/registry/registry.go", "package registry\n")
	if produced, _ := ws.Produced(context.Background(), spec); !produced["scratch/registry"] {
		t.Fatal("a target committed to HEAD after the task is reported as produced")
	}
}

func TestGitWorkspace_AbsentTargetsMakeNoCommit(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, "README", "x\n")
	before, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	commit, err := gitWorkspace{dir: dir}.Prepare(context.Background(), agentbench.TaskSpec{ID: "a", Targets: []string{"scratch/new"}}, 1)
	after, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil || commit != "" || string(before) != string(after) {
		t.Fatalf("nothing to clear is a no-op without an empty commit: %q %v", commit, err)
	}
}

func TestGitWorkspace_RefusesADirtyWorkspace(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, "README", "x\n")
	if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (gitWorkspace{dir: dir}).Prepare(context.Background(), agentbench.TaskSpec{ID: "a"}, 1); err == nil {
		t.Fatal("uncommitted entries must refuse the task")
	}
}

// audit 2026-10-12: GitHub #307. A failed task's backlog deposit left one
// uncommitted BACKLOG.md line in the bench workspace, and Prepare refused with
// "has uncommitted entries", naming neither the path nor the task that left
// it. The refusal now lists the dirty paths and, from each change's
// "via task_..." deposit trailers, the origin task (benchmark LLD §12.25 D4).
func TestGitWorkspace_PrepareNamesDirtyPathsAndOrigin(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, "BACKLOG.md", "# Backlog\n\n- [?] old item (via task_20260901000000_aaaaaaaaaaaaaaaa, 2026-09-01)\n")
	f, err := os.OpenFile(filepath.Join(dir, "BACKLOG.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("- [?] **[bug]** x — y (via task_20260927075535_c5798f15038adcbc, 2026-09-27)\n")
	_ = f.Close()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = gitWorkspace{dir: dir}.Prepare(context.Background(), agentbench.TaskSpec{ID: "sw-01"}, 1)
	if err == nil {
		t.Fatal("a dirty workspace must be refused")
	}
	msg := err.Error()
	for _, want := range []string{"BACKLOG.md", "task_20260927075535_c5798f15038adcbc", "notes.txt", "origin unknown"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name %q:\n%s", want, msg)
		}
	}
	// The committed line's task is history, not the origin of the dirt.
	if strings.Contains(msg, "task_20260901000000_aaaaaaaaaaaaaaaa") {
		t.Errorf("refusal attributes the dirt to a task whose line was already committed:\n%s", msg)
	}
}

// The rollup says whether "task success" means the tasks wrote their code.
func TestWorkspaceLine(t *testing.T) {
	var old agentbench.Journal
	if got := workspaceLine(old); !strings.Contains(got, "NOT reset") {
		t.Fatalf("a journal without a reset must say so: %q", got)
	}
	j := agentbench.Journal{Manifest: agentbench.RunManifest{WorkspaceReset: agentbench.WorkspaceResetTargets},
		TaskRuns: []agentbench.TaskRun{
			{TargetsProduced: map[string]bool{"scratch/a": true}},
			{TargetsProduced: map[string]bool{"scratch/b": false}},
		}}
	if got := workspaceLine(j); !strings.Contains(got, "targets produced 1/2") {
		t.Fatalf("got %q", got)
	}
}

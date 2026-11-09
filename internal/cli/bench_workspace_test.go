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

// GitHub #600 (benchmark LLD §12.31). Found at: e2e-lane (the 2026-10-31
// gemma-4-26b-a4b arm): sw-05-table-test r3 failed and left uncommitted files,
// the next task's Prepare refused the dirty workspace, and the unattended arm
// stopped. Regression test at: integration, Prepare on a real repository.
// Lower because: the e2e lane is the operator's bench host. Leftovers are now
// stashed, archived with the history (§12.30) and named in the root commit.
func TestGitWorkspace_ArchivesAFailedTasksLeftovers_600(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, "README", "x\n")
	commitFile(t, dir, "scratch/old/old.go", "package old\n")
	if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch/old/old.go"), []byte("package old // half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("artifacts/out/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignored := filepath.Join(dir, "artifacts", "out", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(ignored), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignored, []byte("ephemeral"), 0o644); err != nil {
		t.Fatal(err)
	}

	commit, err := gitWorkspace{dir: dir}.Prepare(context.Background(), agentbench.TaskSpec{ID: "dp-01-nilguard"}, 1)
	if err != nil || commit == "" {
		t.Fatalf("a failed task's leftovers must not stop the next task: %q %v", commit, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stray")); !os.IsNotExist(err) {
		t.Error("an untracked leftover must be gone from the tree")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "scratch/old/old.go")); string(b) != "package old\n" {
		t.Errorf("a modified tracked file must be back to HEAD, got %q", b)
	}
	if _, err := os.Stat(ignored); err != nil {
		t.Error("an ignored path must be left alone")
	}
	if status, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); len(status) != 0 {
		t.Fatalf("the task starts from a clean tree: %q", status)
	}
	archive := dir + ".history.git"
	stash := ""
	for _, r := range strings.Split(gitOut(t, "", "--git-dir", archive, "for-each-ref", "--format=%(refname)"), "\n") {
		if strings.HasPrefix(r, "refs/bench/dp-01-nilguard/r1/") && strings.HasSuffix(r, "/refs/stash") {
			stash = r
		}
	}
	if stash == "" {
		t.Fatal("the leftovers must be archived as a stash ref")
	}
	files := gitOut(t, "", "--git-dir", archive, "show", "--name-only", "--format=", stash+"^3")
	if !strings.Contains(files, "stray") {
		t.Errorf("the archived stash must hold the untracked file, got %q", files)
	}
	if diff := gitOut(t, "", "--git-dir", archive, "diff", stash+"^1", stash, "--name-only"); !strings.Contains(diff, "scratch/old/old.go") {
		t.Errorf("the archived stash must hold the tracked change, got %q", diff)
	}
	body := gitOut(t, dir, "log", "-1", "--format=%B")
	if !strings.Contains(body, "Leftovers archived") || !strings.Contains(body, "stray") || !strings.Contains(body, "scratch/old/old.go") {
		t.Fatalf("the root commit must name the archived leftovers:\n%s", body)
	}
}

// Leftovers git cannot stash (unmerged paths from an interrupted merge) are
// refused, not half-archived (#600, §12.31).
func TestGitWorkspace_RefusesAnInterruptedMerge_600(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, "f.txt", "base\n")
	git := func(args ...string) {
		t.Helper()
		_ = exec.Command("git", append([]string{"-C", dir}, args...)...).Run()
	}
	git("checkout", "-q", "-b", "other")
	commitFile(t, dir, "f.txt", "other\n")
	git("checkout", "-q", "master")
	commitFile(t, dir, "f.txt", "master\n")
	git("merge", "-q", "other") // conflicts and stops, leaving unmerged paths
	if out, _ := exec.Command("git", "-C", dir, "ls-files", "-u").Output(); len(out) == 0 {
		t.Fatal("fixture: the merge did not conflict")
	}
	if _, err := (gitWorkspace{dir: dir}).Prepare(context.Background(), agentbench.TaskSpec{ID: "a"}, 1); err == nil {
		t.Fatal("an interrupted merge must refuse the task")
	}
}

// audit 2026-10-12: GitHub #307. A failed task's backlog deposit left one
// uncommitted BACKLOG.md line in the bench workspace, and Prepare refused with
// "has uncommitted entries", naming neither the path nor the task that left
// it. The record now lists the dirty paths and, from each change's
// "via task_..." deposit trailers, the origin task (benchmark LLD §12.25 D4).
// Since #600 (§12.31) that record is the root commit's message, not a refusal.
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

	if _, err := (gitWorkspace{dir: dir}).Prepare(context.Background(), agentbench.TaskSpec{ID: "sw-01"}, 1); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	msg := gitOut(t, dir, "log", "-1", "--format=%B")
	for _, want := range []string{"BACKLOG.md", "task_20260927075535_c5798f15038adcbc", "notes.txt", "origin unknown"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the leftovers record does not name %q:\n%s", want, msg)
		}
	}
	// The committed line's task is history, not the origin of the dirt.
	if strings.Contains(msg, "task_20260901000000_aaaaaaaaaaaaaaaa") {
		t.Errorf("the record attributes the dirt to a task whose line was already committed:\n%s", msg)
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
	if got := workspaceLine(j); !strings.Contains(got, "targets produced 1/2") || !strings.Contains(got, "workflow state carried over") {
		t.Fatalf("a harness-9 reset cleared targets only, and must say so: %q", got)
	}
	// §12.29: the reset that also clears BACKLOG.md and .autonomy/, but left
	// the history (harness 10).
	j.Manifest.WorkspaceReset = agentbench.WorkspaceResetTargetsAndState
	if got := workspaceLine(j); !strings.Contains(got, "targets produced 1/2") || strings.Contains(got, "carried over") ||
		!strings.Contains(got, "git history") {
		t.Fatalf("got %q", got)
	}
	// §12.30: targets, state and history (harness 11): no caveat.
	j.Manifest.WorkspaceReset = agentbench.WorkspaceResetTargetsStateAndHistory
	if got := workspaceLine(j); !strings.Contains(got, "targets produced 1/2") || strings.Contains(got, "carried over") ||
		strings.Contains(got, "git history") {
		t.Fatalf("got %q", got)
	}
}

// GitHub #593 (benchmark LLD §12.28). Found at: e2e-lane (a new bench
// instance's smoke failed before any model call). Regression test at: unit;
// lower because the defect is Prepare's handling of a missing directory.
// The first task on a fresh bench instance found no workspace: the daemon
// creates it only when a task runs, and Prepare runs before.
func TestGitWorkspace_PrepareBootstrapsAMissingWorkspace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspaces", "agentbench")
	spec := agentbench.TaskSpec{ID: "sw-01", Targets: []string{"scratch/x"}}
	if _, err := (gitWorkspace{dir: dir}).Prepare(context.Background(), spec, 1); err != nil {
		t.Fatalf("Prepare on a missing workspace: %v", err)
	}
	out, err := exec.Command("git", "-C", dir, "log", "--format=%s").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "bootstrap: vornik-managed project") {
		t.Fatalf("the workspace was not bootstrapped as the daemon would: %v %s", err, out)
	}
}

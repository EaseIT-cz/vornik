package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/executor"
)

// GitHub #808 (benchmark LLD §12.33). Found at: production (operator: each
// batch of the qwen3.8 polyglot arm took more time and tokens). The reset
// removed only a task's targets and two named state paths, so an
// agent-written lowercase backlog.md, every earlier task's solved exercise
// and an old task set's targets were committed into every later repeat.
// Lower because: the arm's effect needed a 34-hour run; this reproduces the
// workspace state that caused it, on the real git preparer.

func trackedFiles808(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "ls-files").Output()
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Fields(string(out))
	sort.Strings(files)
	return files
}

func TestBenchWorkspace808_ResetKeepsOnlyTheBaseline(t *testing.T) {
	dir := gitRepo(t)
	commitFile(t, dir, ".vornik/bootstrap.md", "edited by an agent\n")
	commitFile(t, dir, "go.mod", "module scratch\n\ngo 1.21\n\nrequire example.com/leak v1.0.0\n")
	commitFile(t, dir, "backlog.md", "| Solve Go exercise: polyglot/go/bowling | done | Reviewer re-ran go test |\n")
	commitFile(t, dir, "polyglot/go/bowling/bowling.go", "package bowling // solved by an earlier task\n")
	commitFile(t, dir, "scratch/semver/semver.go", "package semver // an old task set's target\n")
	commitFile(t, dir, "notes-on-no-list.md", "a name no list knows\n")
	ws := gitWorkspace{dir: dir}

	spec := agentbench.TaskSpec{ID: "pg-python-bowling", Targets: []string{"polyglot/python/bowling"}}
	if _, err := ws.Prepare(context.Background(), spec, 3); err != nil {
		t.Fatal(err)
	}
	got := trackedFiles808(t, dir)
	want := []string{".vornik/bootstrap.md", "go.mod"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the repeat starts from %v, want only the baseline %v", got, want)
	}
	// The baseline files are the harness's, whatever an agent made of them.
	for path, content := range map[string]string{"go.mod": agentbench.BaselineGoMod, ".vornik/bootstrap.md": executor.BootstrapMarkerBody} {
		b, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || string(b) != content {
			t.Errorf("%s = %q (%v), want the harness's content %q", path, b, err, content)
		}
	}
	// What was removed is archived with the history (§12.30), not lost.
	archived, err := exec.Command("git", "--git-dir="+dir+".history.git", "log", "--all", "--format=%H", "--", "backlog.md").Output()
	if err != nil || len(strings.TrimSpace(string(archived))) == 0 {
		t.Fatalf("the history archive does not hold the removed backlog.md (%v)", err)
	}
	if own, _ := exec.Command("git", "-C", dir, "log", "--all", "--format=%H", "--", "backlog.md").Output(); len(strings.TrimSpace(string(own))) != 0 {
		t.Fatal("the workspace's own history still reaches the removed backlog.md")
	}
	if status, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); len(status) != 0 {
		t.Fatalf("the reset leaves a clean workspace: %q", status)
	}
	// Nothing outside the baseline: the next repeat is a no-op.
	if again, err := ws.Prepare(context.Background(), spec, 4); err != nil || again != "" {
		t.Fatalf("a baseline-only workspace needs no reset: %q %v", again, err)
	}
}

// review-20261118-a85d F6: a dirty workspace already at its baseline used to
// stash the leftovers and return early, so the stash stayed in the workspace
// (readable from a step) and never reached the archive.
func TestBenchWorkspace808_LeftoversOnABaselineWorkspaceAreArchived(t *testing.T) {
	dir := gitRepo(t)
	ws := gitWorkspace{dir: dir}
	spec := agentbench.TaskSpec{ID: "t1", Targets: []string{"scratch/x"}}
	if _, err := ws.Prepare(context.Background(), spec, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit, err := ws.Prepare(context.Background(), spec, 2)
	if err != nil || commit == "" {
		t.Fatalf("leftovers must force a reset: %q %v", commit, err)
	}
	if refs, _ := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(refname)").Output(); strings.Contains(string(refs), "refs/stash") {
		t.Fatalf("the stash is still in the workspace: %s", refs)
	}
	archived, err := exec.Command("git", "--git-dir="+dir+".history.git", "for-each-ref", "--format=%(refname)").Output()
	if err != nil || !strings.Contains(string(archived), "/refs/stash") {
		t.Fatalf("the stash did not reach the archive: %v %s", err, archived)
	}
}

package cli

// GitHub #650 phase 1 (benchmark design, Amendment 2026-11-16): a borrowed
// exercise starts from its stubs. Targets are cleared before each repeat
// (§12.23), so the stubs are a seed that Prepare puts into the single target
// after the clear and commits in the reset's root commit.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

func seededSpec(t *testing.T) agentbench.TaskSpec {
	t.Helper()
	base := t.TempDir()
	for path, body := range map[string]string{
		"seed/leap.go": "package leap\n\nfunc IsLeapYear(year int) bool { panic(\"implement\") }\n",
		"seed/go.mod":  "module leap\n\ngo 1.22\n",
	} {
		full := filepath.Join(base, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return agentbench.TaskSpec{ID: "pg-go-leap", Targets: []string{"polyglot/go/leap"}, Seed: "seed"}.WithAttachmentBase(base)
}

func headFiles(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestGitWorkspace_PrepareSeedsTheTargetInTheRootCommit_650(t *testing.T) {
	dir := gitRepo(t)
	ws := gitWorkspace{dir: dir}
	spec := seededSpec(t)

	// A pristine workspace: nothing to reset, but the seed must still land.
	commit, err := ws.Prepare(context.Background(), spec, 1)
	if err != nil || commit == "" {
		t.Fatalf("a seeded task gets a root commit even when nothing needed resetting: %q %v", commit, err)
	}
	files := headFiles(t, dir)
	for _, want := range []string{"polyglot/go/leap/leap.go", "polyglot/go/leap/go.mod"} {
		if !strings.Contains(files, want) {
			t.Errorf("%s is not in the root commit:\n%s", want, files)
		}
	}
	if status, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); len(status) != 0 {
		t.Fatalf("seeding leaves a clean workspace: %q", status)
	}

	// The repeat changed a stub and added a file; the next repeat starts
	// from the seed again and does not mistake it for a leftover.
	commitFile(t, dir, "polyglot/go/leap/leap.go", "package leap\n\nfunc IsLeapYear(year int) bool { return year%4 == 0 }\n")
	commitFile(t, dir, "polyglot/go/leap/leap_test.go", "package leap\n")
	if _, err := ws.Prepare(context.Background(), spec, 2); err != nil {
		t.Fatalf("a repeat after a seeded repeat: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "polyglot/go/leap/leap.go"))
	if err != nil || !strings.Contains(string(got), `panic("implement")`) {
		t.Fatalf("the stub was not restored: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "polyglot/go/leap/leap_test.go")); !os.IsNotExist(err) {
		t.Fatal("a file the last repeat added survived the reset")
	}
	if n := commitObjects(t, dir); n != 1 {
		t.Fatalf("the workspace holds %d commits after the reset, want 1 (§12.30)", n)
	}
}

func TestGitWorkspace_PrepareRefusesASeedOutsideTheTaskSet_650(t *testing.T) {
	ws := gitWorkspace{dir: gitRepo(t)}
	spec := agentbench.TaskSpec{ID: "x", Targets: []string{"polyglot/go/x"}, Seed: "../elsewhere"}.WithAttachmentBase(t.TempDir())
	if _, err := ws.Prepare(context.Background(), spec, 1); err == nil {
		t.Fatal("a seed outside the task-set directory was copied")
	}
}

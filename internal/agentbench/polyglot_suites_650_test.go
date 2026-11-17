package agentbench

// GitHub #650 phase 1 (benchmark design, Amendment 2026-11-16): every
// imported Aider Polyglot suite must pass against Exercism's reference
// solution and fail against the stub the agents start from. A suite that
// cannot pass grades every arm as failed; one that cannot fail grades the
// untouched stub as solved. Both are Exercism's code and run on the host, as
// TestShippedAcceptanceSuites runs ours. The Python suites are unittest
// classes, so the host needs python3 and no pytest.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolyglotSuites_PassTheReferenceAndFailTheStub_650(t *testing.T) {
	if testing.Short() {
		t.Skip("runs every polyglot suite twice")
	}
	raw, err := os.ReadFile("tasksets/polyglot-go-python-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var tasks []TaskSpec
	if err := json.Unmarshal(raw, &tasks); err != nil {
		t.Fatal(err)
	}
	// 39 Go and 34 Python selected; markdown, ledger, counter and
	// robot-simulator are left out (SOURCE.json says why), so 35 Go and 34
	// Python.
	if len(tasks) != 69 {
		t.Fatalf("the set holds %d tasks, want 69", len(tasks))
	}
	if err := ValidateTaskTargets(tasks, true); err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		task := task.WithAttachmentBase("tasksets")
		t.Run(task.ID, func(t *testing.T) {
			t.Parallel()
			seed, _ := task.SeedDir()
			suite, _ := task.AcceptanceDir()
			reference := filepath.Join(filepath.Dir(suite), "reference")
			for variant, wantPass := range map[string]bool{"reference": true, "stub": false} {
				dir := t.TempDir()
				copyTree(t, seed, dir)
				if variant == "reference" {
					copyTree(t, reference, dir)
				}
				copyTree(t, suite, dir)
				passed, out := runSuite(t, task, dir)
				if passed != wantPass {
					t.Errorf("%s: suite vs %s passed=%v, want %v\n%s", task.ID, variant, passed, wantPass, out)
				}
			}
		})
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	files, err := SeedFiles(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(from, rel))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runSuite(t *testing.T, task TaskSpec, dir string) (bool, string) {
	t.Helper()
	var cmd *exec.Cmd
	if strings.HasPrefix(task.ID, "pg-go-") {
		cmd = exec.Command("go", "test", "-count=1", "-run", "^TestAcceptance_", "./...")
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "GOPROXY=off")
	} else {
		files, _ := filepath.Glob(filepath.Join(dir, "*_test.py"))
		args := []string{"-m", "unittest"}
		for _, f := range files {
			args = append(args, filepath.Base(f))
		}
		cmd = exec.Command("python3", args...)
		cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	s := string(out)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return err == nil, s
}

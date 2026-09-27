package agentbench

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Independent grading (benchmark LLD §12.24, 2026-09-26). Incident: the
// slow-hardware arms' "100% success" was the swarm grading itself; nothing
// independent decided whether the code was right.

func TestAcceptanceDir_ResolvesAgainstTheSetAndCannotEscape(t *testing.T) {
	spec := TaskSpec{ID: "a", Acceptance: "testdata/acceptance/a"}.WithAttachmentBase("/sets")
	if got, err := spec.AcceptanceDir(); err != nil || got != "/sets/testdata/acceptance/a" {
		t.Fatalf("AcceptanceDir = %q, %v", got, err)
	}
	for _, bad := range []string{"../x", "/etc", "testdata/../../x"} {
		if _, err := (TaskSpec{ID: "a", Acceptance: bad}.WithAttachmentBase("/sets")).AcceptanceDir(); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if got, err := (TaskSpec{ID: "a"}).AcceptanceDir(); got != "" || err != nil {
		t.Fatalf("no suite is not an error: %q %v", got, err)
	}
}

type fakeGrader struct{ graded []string }

func (f *fakeGrader) Grade(_ context.Context, spec TaskSpec) AcceptanceResult {
	f.graded = append(f.graded, spec.ID)
	return AcceptanceResult{Outcome: AcceptanceFailed, Output: "assertion"}
}

func TestRunner_RecordsAcceptanceAndNotGraded(t *testing.T) {
	g := &fakeGrader{}
	r := &Runner{Tasks: &fakeTasks{}, Traces: &fakeTraces{}, Workspace: &fakeWorkspace{}, Grader: g}
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{
		{ID: "graded", Prompt: "scratch/g", Targets: []string{"scratch/g"}, Acceptance: "testdata/acceptance/graded"},
		{ID: "ungraded", Prompt: "scratch/u", Targets: []string{"scratch/u"}},
	}
	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*AcceptanceResult{}
	for _, run := range j.TaskRuns {
		byID[run.TaskID] = run.Acceptance
	}
	if a := byID["graded"]; a == nil || a.Outcome != AcceptanceFailed {
		t.Fatalf("a graded task records its outcome: %+v", a)
	}
	if a := byID["ungraded"]; a == nil || a.Outcome != AcceptanceNotGraded {
		t.Fatalf("a task without a suite is not_graded, never passed: %+v", a)
	}
	if len(g.graded) != 1 || g.graded[0] != "graded" {
		t.Fatalf("only a task with a suite is graded: %v", g.graded)
	}
}

func TestAcceptanceSetDigest_OrderIndependentAndSensitive(t *testing.T) {
	base := t.TempDir()
	mk := func(id, body string) TaskSpec {
		dir := filepath.Join(base, id)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "zz_acceptance_test.go"), []byte(body), 0o644)
		return TaskSpec{ID: id, Acceptance: id}.WithAttachmentBase(base)
	}
	a, b := mk("a", "package a"), mk("b", "package b")
	d1, err1 := AcceptanceSetDigest([]TaskSpec{a, b})
	d2, err2 := AcceptanceSetDigest([]TaskSpec{b, a})
	if err1 != nil || err2 != nil || d1 != d2 || d1 == "" {
		t.Fatalf("order-independent: %q %q %v %v", d1, d2, err1, err2)
	}
	empty, _ := AcceptanceSetDigest([]TaskSpec{{ID: "x"}})
	if empty == d1 || empty == "" {
		t.Fatal("an ungraded set has its own, non-empty digest, distinct from a graded one")
	}
	_ = os.WriteFile(filepath.Join(base, "a", "zz_acceptance_test.go"), []byte("package a // changed"), 0o644)
	if d3, _ := AcceptanceSetDigest([]TaskSpec{a, b}); d3 == d1 {
		t.Fatal("changing a test must change the digest")
	}
}

// Every shipped suite must pass against its reference solution and fail
// against its deliberately broken variant: a suite that cannot pass, or
// cannot fail, is broken. Both are OUR code, so this runs on the host.
func TestShippedAcceptanceSuites(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go test on each suite")
	}
	// requireTaskSets skips in a CE checkout, where the export strips the
	// EE-only task sets; it fails if they exist but the set is missing.
	raw, err := os.ReadFile(requireTaskSets(t, "tasksets/dev-swarm-tasks-v1.json")[0])
	if err != nil {
		t.Fatal(err)
	}
	var tasks []TaskSpec
	if err := json.Unmarshal(raw, &tasks); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, task := range tasks {
		if task.Acceptance == "" {
			continue
		}
		n++
		suite := filepath.Join("tasksets", task.Acceptance)
		pkg := filepath.Base(task.Targets[0])
		for variant, wantPass := range map[string]bool{"reference": true, "broken": false} {
			dir := t.TempDir()
			for _, src := range []string{suite, filepath.Join(suite, variant)} {
				files, _ := filepath.Glob(filepath.Join(src, "*.go"))
				for _, f := range files {
					b, _ := os.ReadFile(f)
					_ = os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0o644)
				}
			}
			_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module scratch/"+pkg+"\n\ngo 1.25\n"), 0o644)
			cmd := exec.Command("go", "test", "-count=1", "./...")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if passed := err == nil; passed != wantPass {
				t.Errorf("%s suite vs %s: passed=%v, want %v\n%s", task.ID, variant, passed, wantPass, strings.TrimSpace(string(out)))
			}
		}
	}
	if n < 4 {
		t.Fatalf("found %d shipped suites, want at least the 4 slow-hardware tasks", n)
	}
}

// Changing a suite changes what "passed" means: the arms are incomparable.
func TestCheckComparable_RefusesDifferentAcceptanceSuites(t *testing.T) {
	a, b := baseArm(), baseArm()
	a.AcceptanceSHA256, b.AcceptanceSHA256 = "aaa", "bbb"
	if err := CheckComparable(a, b); err == nil || !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("different suites must be refused: %v", err)
	}
	b.AcceptanceSHA256 = ""
	if err := CheckComparable(a, b); err == nil {
		t.Fatal("a graded arm against an ungraded one must be refused")
	}
}

// Verdicts carried forward by rescore are only honest while the suites are
// the ones that produced them (§12.24).
func TestRescore_RefusesWhenTheSuitesChanged(t *testing.T) {
	base := t.TempDir()
	_ = os.MkdirAll(filepath.Join(base, "a"), 0o755)
	_ = os.WriteFile(filepath.Join(base, "a", "zz_acceptance_test.go"), []byte("package a // now"), 0o644)
	tasks := []TaskSpec{TaskSpec{ID: "a", Acceptance: "a"}.WithAttachmentBase(base)}
	j := Journal{}
	j.Manifest.RunID = "r1"
	j.Manifest.Arm.HarnessVersion = "8"
	j.Manifest.Arm.AcceptanceSHA256 = "digest-of-the-suites-that-graded-it"
	_, err := RescoreWithTasks(context.Background(), j, snapTraces{}, []Probe{SchemaProbe{}}, nil, tasks)
	if err == nil || !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("changed suites must refuse the rescore: %v", err)
	}
}

// A graded journal re-scored with no task set cannot be checked against its
// suites at all, so it is refused like one carrying task scores (review
// review-20260926-b46d F5).
func TestRescore_RefusesAGradedJournalWithoutTheTaskSet(t *testing.T) {
	j := Journal{}
	j.Manifest.RunID = "r1"
	j.Manifest.Arm.HarnessVersion = "8"
	j.Manifest.Arm.AcceptanceSHA256 = "digest-of-the-suites-that-graded-it"
	_, err := RescoreWithTasks(context.Background(), j, snapTraces{}, []Probe{SchemaProbe{}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("a graded journal without its task set must refuse the rescore: %v", err)
	}
	if !strings.Contains(err.Error(), "task set") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A suite is copied into ONE package; a task grading several targets would be
// silently partial (review-20260926-b46d F6).
func TestValidateTaskTargets_AcceptanceNeedsExactlyOneTarget(t *testing.T) {
	for _, targets := range [][]string{{}, {"scratch/a", "scratch/b"}} {
		task := TaskSpec{ID: "t", Prompt: "write scratch/a and scratch/b", Targets: targets, Acceptance: "suite"}
		if err := ValidateTaskTargets([]TaskSpec{task}, false); err == nil {
			t.Fatalf("an acceptance suite with targets %v must be refused", targets)
		}
	}
	ok := TaskSpec{ID: "t", Prompt: "write scratch/a", Targets: []string{"scratch/a"}, Acceptance: "suite"}
	if err := ValidateTaskTargets([]TaskSpec{ok}, false); err != nil {
		t.Fatal(err)
	}
}

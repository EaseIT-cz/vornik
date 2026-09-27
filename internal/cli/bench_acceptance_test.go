package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

// Independent grading (benchmark LLD §12.24): the produced package is graded
// with a suite the agents never saw, inside the agent image. Incident: the
// slow-hardware arms' "100% success" was the swarm grading itself.

type fakeGradeRunner struct {
	calls   [][]string
	testOut string
	testErr error
}

func (f *fakeGradeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "podman" {
		return []byte(f.testOut), f.testErr
	}
	return nil, nil
}

func acceptanceSpec(t *testing.T) agentbench.TaskSpec {
	t.Helper()
	base := t.TempDir()
	suite := filepath.Join(base, "suite")
	if err := os.MkdirAll(suite, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(suite, "zz_acceptance_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return agentbench.TaskSpec{ID: "t", Targets: []string{"scratch/x"}, Acceptance: "suite"}.WithAttachmentBase(base)
}

func newTestGrader(t *testing.T, f *fakeGradeRunner) podmanGrader {
	t.Helper()
	ws := gitRepo(t)
	commitFile(t, ws, "scratch/x/x.go", "package x\n")
	return podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: f.run}
}

func TestPodmanGrader_ClassifiesOutcomes(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want agentbench.AcceptanceOutcome
	}{
		{"pass", "ok  \tscratch/x\t0.01s", nil, agentbench.AcceptancePassed},
		{"assertion", "--- FAIL: TestAcceptance_X\nFAIL\tscratch/x", errors.New("exit status 1"), agentbench.AcceptanceFailed},
		{"hang", "panic: test timed out after 4m0s\nFAIL\tscratch/x", errors.New("exit status 1"), agentbench.AcceptanceTimeout},
		{"build", "# scratch/x\n./x.go:3:2: undefined: New\nFAIL\tscratch/x [build failed]", errors.New("exit status 1"), agentbench.AcceptanceDoesNotCompile},
		{"podman", "Error: image not known", errors.New("exit status 125"), agentbench.AcceptanceError},
		// A non-zero exit with no failing test in the output is not a model
		// failure (review-20260926-b46d F4).
		{"unrecognised", "signal: killed", errors.New("exit status 2"), agentbench.AcceptanceError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeGradeRunner{testOut: c.out, testErr: c.err}
			res := newTestGrader(t, f).Grade(context.Background(), acceptanceSpec(t))
			if res.Outcome != c.want {
				t.Fatalf("outcome = %s, want %s (%s)", res.Outcome, c.want, res.Output)
			}
			if c.want != agentbench.AcceptancePassed && !strings.Contains(res.Output, strings.Split(c.out, "\n")[0]) {
				t.Fatalf("a non-pass keeps its output: %q", res.Output)
			}
		})
	}
}

func TestPodmanGrader_RunsSandboxedAndNeverOnTheHost(t *testing.T) {
	f := &fakeGradeRunner{testOut: "ok"}
	_ = newTestGrader(t, f).Grade(context.Background(), acceptanceSpec(t))
	var podman []string
	for _, c := range f.calls {
		if c[0] == "go" {
			t.Fatal("agent-written code must never run on the host")
		}
		if c[0] == "podman" {
			podman = c
		}
	}
	joined := strings.Join(podman, " ")
	for _, want := range []string{"--network=none", "--pull=never", "--rm", "--entrypoint go", "localhost/vornik-agent:bench", "-timeout=4m", "GOPROXY=off", "-run=^TestAcceptance_"} {
		if !strings.Contains(joined, want) {
			t.Errorf("podman call lacks %q: %s", want, joined)
		}
	}
}

func TestPodmanGrader_MissingPackageDoesNotCompile(t *testing.T) {
	f := &fakeGradeRunner{testOut: "ok"}
	g := newTestGrader(t, f)
	spec := acceptanceSpec(t)
	spec.Targets = []string{"scratch/absent"}
	if res := g.Grade(context.Background(), spec); res.Outcome != agentbench.AcceptanceDoesNotCompile {
		t.Fatalf("a missing package is does_not_compile: %+v", res)
	}
	for _, c := range f.calls {
		if c[0] == "podman" {
			t.Fatal("nothing to test: podman must not run")
		}
	}
}

func TestAcceptanceLine(t *testing.T) {
	j := agentbench.Journal{TaskRuns: []agentbench.TaskRun{
		{Acceptance: &agentbench.AcceptanceResult{Outcome: agentbench.AcceptancePassed}},
		{Acceptance: &agentbench.AcceptanceResult{Outcome: agentbench.AcceptanceFailed}},
		{Acceptance: &agentbench.AcceptanceResult{Outcome: agentbench.AcceptanceNotGraded}},
		{Acceptance: &agentbench.AcceptanceResult{Outcome: agentbench.AcceptanceError}},
	}}
	got := acceptanceLine(j)
	if !strings.Contains(got, "graded 2 of 4") || !strings.Contains(got, "passed 1") || !strings.Contains(got, "harness errors 1") {
		t.Fatalf("got %q", got)
	}
	if got := acceptanceLine(agentbench.Journal{}); !strings.Contains(got, "not graded") {
		t.Fatalf("a journal with no grading says so: %q", got)
	}
}

// Suites and attachments resolve against the task-set file (they were
// resolving against the working directory and pointing at nothing).
func TestLoadTaskSet_ResolvesAgainstTheSetDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "set.json")
	if err := os.WriteFile(p, []byte(`[{"id":"a","prompt":"p","acceptance":"suites/a"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks, err := loadTaskSet(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := tasks[0].AcceptanceDir(); got != filepath.Join(dir, "suites/a") {
		t.Fatalf("AcceptanceDir = %q, want it under the set's directory", got)
	}
}

// A set with suites and a workspace but no image refuses to run ungraded.
func TestBenchGrader_FailsClosedWithoutAnImage(t *testing.T) {
	old := [2]string{benchAgentWorkspace, benchAgentAcceptanceImage}
	defer func() { benchAgentWorkspace, benchAgentAcceptanceImage = old[0], old[1] }()
	benchAgentWorkspace, benchAgentAcceptanceImage = "/ws", ""
	if _, err := benchGrader([]agentbench.TaskSpec{{ID: "a", Acceptance: "s"}}); err == nil {
		t.Fatal("suites without --acceptance-image must be refused")
	}
	if g, err := benchGrader([]agentbench.TaskSpec{{ID: "a"}}); err != nil || g != nil {
		t.Fatalf("no suites and no image: no grader, no error: %v %v", g, err)
	}
	benchAgentAcceptanceImage = "img"
	if g, err := benchGrader([]agentbench.TaskSpec{{ID: "a", Acceptance: "s"}}); err != nil || g == nil {
		t.Fatalf("an image and a workspace grade: %v %v", g, err)
	}
}

// The grade is of the COMMITTED HEAD, which after a completed task includes the
// agent's merged work: the suite lands beside it, uncommitted edits do not
// (review-20260926-b46d F1).
func TestPodmanGrader_GradesTheCommittedTreeWithTheSuite(t *testing.T) {
	var seen []string
	f := &fakeGradeRunner{testOut: "ok"}
	g := newTestGrader(t, f)
	if err := os.WriteFile(filepath.Join(g.workspace, "scratch/x/uncommitted.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for _, a := range args {
			if host, ok := strings.CutSuffix(a, ":/work:z"); ok {
				entries, _ := os.ReadDir(filepath.Join(host, "scratch/x"))
				for _, e := range entries {
					seen = append(seen, e.Name())
				}
			}
		}
		return f.run(ctx, name, args...)
	}
	if res := g.Grade(context.Background(), acceptanceSpec(t)); res.Outcome != agentbench.AcceptancePassed {
		t.Fatalf("grade: %+v", res)
	}
	got := strings.Join(seen, ",")
	if got != "x.go,zz_acceptance_test.go" {
		t.Fatalf("graded tree = %s, want the committed package plus the suite only", got)
	}
}

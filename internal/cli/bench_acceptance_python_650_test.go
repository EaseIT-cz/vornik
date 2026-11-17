package cli

// GitHub #650 phase 1 (benchmark design, Amendment 2026-11-16): the hidden
// acceptance grader learns Python, for the Aider Polyglot exercises. Only the
// hidden suite is collected, so the agents' own tests are never the grade;
// the outcome is decided by pytest's exit code plus an output marker, pinned
// against output captured from pytest 7.4.4 in the agent image.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"vornik.io/vornik/internal/agentbench"
)

func pytestOut(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pytest-7.4.4", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// pythonSpec: a task whose suite is affine_cipher_test.py and whose seed is
// the stub module, graded against a workspace that committed the module.
func pythonSpec(t *testing.T, commitModule bool) (agentbench.TaskSpec, string) {
	t.Helper()
	base := t.TempDir()
	for path, body := range map[string]string{
		"suite/affine_cipher_test.py": "from affine_cipher import encode\n",
		"seed/affine_cipher.py":       "def encode(text, key): pass\n",
	} {
		full := filepath.Join(base, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := gitRepo(t)
	if commitModule {
		commitFile(t, ws, "polyglot/python/affine-cipher/affine_cipher.py", "def encode(text, key): return text\n")
	} else {
		commitFile(t, ws, "polyglot/python/affine-cipher/notes.txt", "nothing here\n")
	}
	// The pipeline's own test beside it, with the SAME name as the suite:
	// never collected, never overwritten.
	commitFile(t, ws, "polyglot/python/affine-cipher/affine_cipher_test.py", "def test_mine(): assert False\n")
	spec := agentbench.TaskSpec{ID: "pg-py-affine-cipher", Targets: []string{"polyglot/python/affine-cipher"},
		Acceptance: "suite", Seed: "seed"}.WithAttachmentBase(base)
	return spec, ws
}

func TestPodmanGrader_Python_ClassifiesRealPytestOutput_650(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want agentbench.AcceptanceOutcome
	}{
		{"pass", pytestOut(t, "pass"), nil, agentbench.AcceptancePassed},
		{"assertion", pytestOut(t, "fail"), errors.New("exit status 1"), agentbench.AcceptanceFailed},
		{"syntax error", pytestOut(t, "syntax"), errors.New("exit status 2"), agentbench.AcceptanceDoesNotCompile},
		{"import error", pytestOut(t, "import"), errors.New("exit status 2"), agentbench.AcceptanceDoesNotCompile},
		{"hang", "", errors.New("exit status 124"), agentbench.AcceptanceTimeout},
		{"interrupted without the collection marker", "!!! KeyboardInterrupt !!!", errors.New("exit status 2"), agentbench.AcceptanceError},
		{"exit 1 without a FAILED line", "something odd", errors.New("exit status 1"), agentbench.AcceptanceError},
		{"internal error", "INTERNALERROR>", errors.New("exit status 3"), agentbench.AcceptanceError},
		{"usage error: our suite is missing", "ERROR: file or directory not found", errors.New("exit status 4"), agentbench.AcceptanceError},
		{"no tests collected: a broken suite", "no tests ran", errors.New("exit status 5"), agentbench.AcceptanceError},
		{"podman failed", "Error: image not known", errors.New("exit status 125"), agentbench.AcceptanceError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, ws := pythonSpec(t, true)
			f := &fakeGradeRunner{testOut: c.out, testErr: c.err}
			g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: f.run}
			if got := g.Grade(t.Context(), spec); got.Outcome != c.want {
				t.Fatalf("outcome = %s, want %s (output %q)", got.Outcome, c.want, got.Output)
			}
		})
	}
}

func TestPodmanGrader_Python_RunsOnlyTheHiddenSuiteByName_650(t *testing.T) {
	spec, ws := pythonSpec(t, true)
	f := &fakeGradeRunner{testOut: pytestOut(t, "pass")}
	g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: f.run}
	if got := g.Grade(t.Context(), spec); got.Outcome != agentbench.AcceptancePassed {
		t.Fatalf("outcome = %s: %s", got.Outcome, got.Output)
	}
	var argv []string
	for _, c := range f.calls {
		if c[0] == "podman" {
			argv = c
		}
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--network=none", "--entrypoint timeout", " 240 python -m pytest -q -p no:cacheprovider ",
		"zz_acceptance_affine_cipher_test.py"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "*") {
		t.Errorf("argv carries a glob, which no shell would expand: %s", joined)
	}
	if strings.HasSuffix(joined, " affine_cipher_test.py") || strings.Contains(joined, " affine_cipher_test.py ") {
		t.Errorf("the pipeline's own affine_cipher_test.py is collected: %s", joined)
	}
}

func TestPodmanGrader_Python_MissingStubModuleDoesNotCompile_650(t *testing.T) {
	spec, ws := pythonSpec(t, false)
	f := &fakeGradeRunner{testOut: pytestOut(t, "pass")}
	g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: f.run}
	got := g.Grade(t.Context(), spec)
	if got.Outcome != agentbench.AcceptanceDoesNotCompile || !strings.Contains(got.Output, "affine_cipher.py") {
		t.Fatalf("outcome = %s (%q), want does_not_compile naming the module", got.Outcome, got.Output)
	}
	for _, c := range f.calls {
		if c[0] == "podman" {
			t.Fatal("pytest ran although the module is missing")
		}
	}
}

func TestPodmanGrader_FeedbackOutputIsFullUpToItsCapAndNeverJournaled_650(t *testing.T) {
	spec, ws := pythonSpec(t, true)
	long := strings.Repeat("E   assert 1 == 2\n", 1000) + "FAILED zz_acceptance_affine_cipher_test.py::test_x\n" // ~19 KB
	f := &fakeGradeRunner{testOut: long, testErr: errors.New("exit status 1")}
	g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: f.run}
	got := g.Grade(t.Context(), spec)
	if got.Outcome != agentbench.AcceptanceFailed {
		t.Fatalf("outcome = %s", got.Outcome)
	}
	if len(got.Output) > acceptanceOutputLimit+len("\n[truncated]") {
		t.Errorf("journaled output is %d bytes", len(got.Output))
	}
	if len(got.FeedbackOutput) != len(long) {
		t.Errorf("feedback output is %d bytes, want the full %d", len(got.FeedbackOutput), len(long))
	}
	huge := strings.Repeat("x", feedbackOutputLimit+5000)
	f2 := &fakeGradeRunner{testOut: huge + "\nFAILED a::b\n", testErr: errors.New("exit status 1")}
	g2 := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: f2.run}
	if fb := g2.Grade(t.Context(), spec).FeedbackOutput; len(fb) > feedbackOutputLimit+len("\n[truncated]") {
		t.Errorf("feedback output is %d bytes, cap %d", len(fb), feedbackOutputLimit)
	}
	// Never journaled: the field has no JSON form.
	run := agentbench.TaskRun{TaskID: "t", Acceptance: &got}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if b := string(raw); strings.Contains(b, "FeedbackOutput") || strings.Contains(b, "feedback") ||
		len(b) > acceptanceOutputLimit+512 {
		t.Errorf("the journal carries the feedback output: %d bytes", len(b))
	}
}

func TestPodmanGrader_Python_AHelperKeepsItsNameAndIsNotRun_650(t *testing.T) {
	spec, ws := pythonSpec(t, true)
	suite, _ := spec.AcceptanceDir()
	if err := os.WriteFile(filepath.Join(suite, "test_utils.py"), []byte("class MockSock: pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var argv []string
	var helper []byte
	f := &fakeGradeRunner{testOut: pytestOut(t, "pass")}
	g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "podman" {
			argv = args
			for i, a := range args {
				if a == "-v" && i+1 < len(args) {
					tree := strings.SplitN(args[i+1], ":", 2)[0]
					helper, _ = os.ReadFile(filepath.Join(tree, "polyglot/python/affine-cipher/test_utils.py"))
				}
			}
		}
		return f.run(ctx, name, args...)
	}}
	if got := g.Grade(t.Context(), spec); got.Outcome != agentbench.AcceptancePassed {
		t.Fatalf("outcome = %s: %s", got.Outcome, got.Output)
	}
	if !strings.Contains(string(helper), "MockSock") {
		t.Fatal("the helper is not in the target under its own name")
	}
	if joined := strings.Join(argv, " "); strings.Contains(joined, "test_utils.py") {
		t.Fatalf("the helper was passed to pytest as a test: %s", joined)
	}
}

// Code review review-20261116-a5b7 H1: for a borrowed (seeded) Go item the
// pipeline's own test files are removed from the graded copy, so a broken
// self-test cannot fail the item and an agent-written TestAcceptance_* cannot
// join the grade; the exercise is graded by its own tests alone.
func TestPodmanGrader_SeededGoItemIgnoresThePipelinesTests_650(t *testing.T) {
	spec := seededSpec(t) // pg-go-leap with a seed
	base := filepath.Dir(mustSeedDir(t, spec))
	if err := os.MkdirAll(filepath.Join(base, "suite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "suite", "zz_acceptance_leap_test.go"), []byte("package leap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.Acceptance = "suite"
	ws := gitRepo(t)
	commitFile(t, ws, "polyglot/go/leap/leap.go", "package leap\n")
	commitFile(t, ws, "polyglot/go/leap/mine_test.go", "package leap\n\nfunc TestAcceptance_Mine(t *testing.T) { t.Fatal() }\n")
	var tree string
	var seen []string
	g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "podman" {
			for i, a := range args {
				if a == "-v" && i+1 < len(args) {
					tree = strings.SplitN(args[i+1], ":", 2)[0]
					m, _ := filepath.Glob(filepath.Join(tree, "polyglot/go/leap/*_test.go"))
					for _, f := range m {
						seen = append(seen, filepath.Base(f))
					}
				}
			}
		}
		return []byte("ok  \tleap\t0.01s"), nil
	}}
	if got := g.Grade(t.Context(), spec); got.Outcome != agentbench.AcceptancePassed {
		t.Fatalf("outcome = %s: %s", got.Outcome, got.Output)
	}
	if len(seen) != 1 || seen[0] != "zz_acceptance_leap_test.go" {
		t.Fatalf("test files graded: %v, want only the suite", seen)
	}
}

// The Python twin of H1: a conftest.py or pytest configuration the pipeline
// wrote would be loaded by pytest and could change the grade; it is removed.
func TestPodmanGrader_Python_IgnoresThePipelinesPytestConfiguration_650(t *testing.T) {
	spec, ws := pythonSpec(t, true)
	commitFile(t, ws, "polyglot/python/affine-cipher/conftest.py", "import pytest\n")
	commitFile(t, ws, "polyglot/python/affine-cipher/pytest.ini", "[pytest]\naddopts = -x\n")
	commitFile(t, ws, "pyproject.toml", "[tool.pytest.ini_options]\n")
	var left []string
	g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "podman" {
			for i, a := range args {
				if a == "-v" && i+1 < len(args) {
					tree := strings.SplitN(args[i+1], ":", 2)[0]
					for _, f := range []string{"polyglot/python/affine-cipher/conftest.py", "polyglot/python/affine-cipher/pytest.ini", "pyproject.toml"} {
						if _, err := os.Stat(filepath.Join(tree, f)); err == nil {
							left = append(left, f)
						}
					}
				}
			}
		}
		return []byte(pytestOut(t, "pass")), nil
	}}
	if got := g.Grade(t.Context(), spec); got.Outcome != agentbench.AcceptancePassed {
		t.Fatalf("outcome = %s: %s", got.Outcome, got.Output)
	}
	if len(left) != 0 {
		t.Fatalf("pytest would load the pipeline's %v", left)
	}
}

// review-20261116-a5b7 M2: a suite is Python only when it holds *_test.py,
// and a suite mixing Go and Python tests is refused, never half-graded.
func TestPodmanGrader_AMixedSuiteIsAHarnessError_650(t *testing.T) {
	spec, ws := pythonSpec(t, true)
	suite, _ := spec.AcceptanceDir()
	if err := os.WriteFile(filepath.Join(suite, "zz_acceptance_x_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeGradeRunner{testOut: pytestOut(t, "pass")}
	g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: f.run}
	if got := g.Grade(t.Context(), spec); got.Outcome != agentbench.AcceptanceError {
		t.Fatalf("a mixed suite graded %s", got.Outcome)
	}
}

// review-20261116-8777 F1: the removal keys on the suite's own file names, not
// on the zz_acceptance_ prefix, so a pipeline file that guesses the prefix is
// removed too.
func TestPodmanGrader_SeededGoItemRemovesAGuessedSuiteName_650(t *testing.T) {
	spec := seededSpec(t)
	base := filepath.Dir(mustSeedDir(t, spec))
	if err := os.MkdirAll(filepath.Join(base, "suite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "suite", "zz_acceptance_leap_test.go"), []byte("package leap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.Acceptance = "suite"
	ws := gitRepo(t)
	commitFile(t, ws, "polyglot/go/leap/leap.go", "package leap\n")
	commitFile(t, ws, "polyglot/go/leap/zz_acceptance_evil_test.go", "package leap\n\nfunc TestAcceptance_Evil(t *testing.T) {}\n")
	var seen []string
	g := podmanGrader{workspace: ws, image: "localhost/vornik-agent:bench", run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "podman" {
			for i, a := range args {
				if a == "-v" && i+1 < len(args) {
					m, _ := filepath.Glob(filepath.Join(strings.SplitN(args[i+1], ":", 2)[0], "polyglot/go/leap/*_test.go"))
					for _, f := range m {
						seen = append(seen, filepath.Base(f))
					}
				}
			}
		}
		return []byte("ok  \tleap\t0.01s"), nil
	}}
	g.Grade(t.Context(), spec)
	if len(seen) != 1 || seen[0] != "zz_acceptance_leap_test.go" {
		t.Fatalf("test files graded: %v, want only the suite's", seen)
	}
}

// review-20261116-8777 F7: the fed-back output is cut on a rune boundary.
func TestFeedbackOutput_StaysValidUTF8_650(t *testing.T) {
	out := "a" + strings.Repeat("é", feedbackOutputLimit) // 2 bytes each, offset by one so the cap falls inside a rune
	fb := feedbackOutput(out, agentbench.AcceptanceFailed)
	if !utf8.ValidString(fb) {
		t.Fatal("the fed-back output ends inside a rune")
	}
}

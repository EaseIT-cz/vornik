package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/spawn"
)

// The Python branch of the hidden acceptance grader (benchmark design,
// Amendment 2026-11-16, GitHub #650): the Aider Polyglot Python exercises are
// graded by their own pytest suites, inside the agent image, without network.

// suiteLanguage is "python" for a suite of *_test.py files and "go"
// otherwise; a suite holding both Go and Python tests is refused, never
// half-graded (code review review-20261116-a5b7 M2).
func suiteLanguage(files []string) (string, error) {
	var goTests, pyTests bool
	for _, f := range files {
		goTests = goTests || strings.HasSuffix(f, "_test.go")
		pyTests = pyTests || strings.HasSuffix(f, "_test.py")
	}
	if goTests && pyTests {
		return "", errors.New("the acceptance suite mixes Go and Python tests")
	}
	if pyTests {
		return "python", nil
	}
	return "go", nil
}

// pytestConfigFiles are the files pytest loads on its own from the
// directory it runs in and its parents. A pipeline-written one could change
// the grade, so they are removed from the graded copy.
var pytestConfigFiles = []string{"conftest.py", "pytest.ini", "tox.ini", "setup.cfg", "pyproject.toml"}

// removePytestConfiguration deletes those files from the target and every
// directory above it up to the exported tree's root.
func removePytestConfiguration(tree, pkg string) error {
	for dir := pkg; ; dir = filepath.Dir(dir) {
		for _, name := range pytestConfigFiles {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if dir == tree || !strings.HasPrefix(dir, tree) {
			return nil
		}
	}
}

// removePipelineGoTests deletes every *_test.go in a borrowed item's package
// that is not a suite file, so the item is graded by the exercise's own tests
// alone: a broken self-test cannot fail it and an agent-written
// TestAcceptance_* cannot join it (code review review-20261116-a5b7 H1).
// Vornik's own graded tasks keep §12.24's rule that the pipeline's tests must
// compile, so their figures stay comparable.
// The removal keys on the suite's own file names, never a prefix the
// pipeline could guess (review-20261116-8777 F1); the suite is copied after it.
func removePipelineGoTests(pkg string, suiteFiles []string) error {
	suite := make(map[string]bool, len(suiteFiles))
	for _, f := range suiteFiles {
		suite[filepath.Base(f)] = true
	}
	tests, err := filepath.Glob(filepath.Join(pkg, "*_test.go"))
	if err != nil {
		return err
	}
	for _, f := range tests {
		if suite[filepath.Base(f)] {
			continue // overwritten by the suite's own file next
		}
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	return nil
}

// gradePython checks the produced modules exist, copies each suite file into
// the target as zz_acceptance_<name>, and runs pytest on exactly those files.
// The copied names cannot collide with a test file the pipeline wrote, and
// nothing else is collected, so the agents' own tests are never the grade
// (§12.24).
func (g podmanGrader) gradePython(ctx context.Context, spec agentbench.TaskSpec, tree, pkg string, suiteFiles []string) agentbench.AcceptanceResult {
	if missing, err := missingPythonModules(spec, pkg); err != nil {
		return acceptanceHarnessError(err)
	} else if missing != "" {
		return agentbench.AcceptanceResult{Outcome: agentbench.AcceptanceDoesNotCompile,
			Output: fmt.Sprintf("%s is missing from %s at the workspace HEAD", missing, spec.Targets[0])}
	}
	if err := removePytestConfiguration(tree, pkg); err != nil {
		return acceptanceHarnessError(err)
	}
	var copied []string
	for _, f := range suiteFiles {
		if !strings.HasSuffix(f, ".py") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return acceptanceHarnessError(err)
		}
		// A test file gets the prefix and is run by name. A helper the tests
		// import (paasio's test_utils.py) keeps its own name, since the tests
		// import it by that name, and is not run as a test.
		name := filepath.Base(f)
		isTest := strings.HasSuffix(name, "_test.py")
		if isTest {
			name = "zz_acceptance_" + name
		}
		if err := os.WriteFile(filepath.Join(pkg, name), data, 0o644); err != nil {
			return acceptanceHarnessError(err)
		}
		if isTest {
			copied = append(copied, name)
		}
	}
	if len(copied) == 0 {
		return acceptanceHarnessError(fmt.Errorf("acceptance suite of %q holds no *_test.py file", spec.ID))
	}
	runCtx, cancel := context.WithTimeout(ctx, acceptanceOuterTimeout)
	defer cancel()
	run := g.run
	if run == nil {
		run = execCombined
	}
	argv := append([]string{"run", "--rm", "--network=none", "--pull=never", "--userns=keep-id"},
		spawn.MemoryLimitFlags(g.memoryLimit)...)
	argv = append(argv, "--entrypoint", "timeout",
		"-v", tree+":/work:z", "-w", "/work/"+filepath.ToSlash(filepath.Clean(spec.Targets[0])),
		"-e", "PYTHONDONTWRITEBYTECODE=1",
		g.image, pytestTimeoutSeconds, "python", "-m", "pytest", "-q", "-p", "no:cacheprovider")
	// No shell runs this, so no glob: each copied file by name.
	argv = append(argv, copied...)
	out, err := run(runCtx, "podman", argv...)
	res := classifyPytest(runCtx, string(out), err)
	res.FeedbackOutput = feedbackOutput(string(out), res.Outcome)
	return res
}

// missingPythonModules names the first module the task's seed provides that
// is not in the exported HEAD target; "" when all are there. Without a seed,
// the target must hold at least one Python file.
func missingPythonModules(spec agentbench.TaskSpec, pkg string) (string, error) {
	dir, err := spec.SeedDir()
	if err != nil {
		return "", err
	}
	if dir == "" {
		if py, _ := filepath.Glob(filepath.Join(pkg, "*.py")); len(py) == 0 {
			return "a Python module", nil
		}
		return "", nil
	}
	files, err := agentbench.SeedFiles(dir)
	if err != nil {
		return "", err
	}
	for _, rel := range files {
		if !strings.HasSuffix(rel, ".py") || strings.HasSuffix(rel, "_test.py") {
			continue
		}
		if _, err := os.Stat(filepath.Join(pkg, filepath.FromSlash(rel))); err != nil {
			return rel, nil
		}
	}
	return "", nil
}

var exitStatusRe = regexp.MustCompile(`exit status (\d+)`)

// exitCode reads a process's exit code from an *exec.ExitError, or from the
// "exit status N" text the command seam's errors carry. -1 when there is none.
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if err != nil {
		if m := exitStatusRe.FindStringSubmatch(err.Error()); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return -1
}

// classifyPytest maps pytest's result onto the §12.24 outcomes, by exit code
// plus an output marker, so an exit no row names is never charged to the
// model. The markers are pinned against pytest 7.4.4's real output
// (testdata/pytest-7.4.4).
func classifyPytest(ctx context.Context, out string, err error) agentbench.AcceptanceResult {
	res := agentbench.AcceptanceResult{Output: truncateAcceptance(out)}
	if err == nil {
		return agentbench.AcceptanceResult{Outcome: agentbench.AcceptancePassed}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Outcome = agentbench.AcceptanceError // the OUTER bound: a harness fault
		return res
	}
	switch code := exitCode(err); {
	case code == 1 && strings.Contains(out, "FAILED "):
		res.Outcome = agentbench.AcceptanceFailed
	case code == 2 && (strings.Contains(out, "ERROR collecting") || strings.Contains(out, "error during collection")):
		res.Outcome = agentbench.AcceptanceDoesNotCompile
	case code == 124:
		res.Outcome = agentbench.AcceptanceTimeout
	default:
		// 2 without the collection marker (interrupted), 3 (internal), 4
		// (usage: our suite is missing), 5 (no tests: a broken suite),
		// podman's own 125-127, and anything else: a harness fault.
		res.Outcome = agentbench.AcceptanceError
	}
	return res
}

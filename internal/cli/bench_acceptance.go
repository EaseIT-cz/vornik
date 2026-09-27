package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentbench"
)

// podmanGrader grades a task's produced package with its hidden acceptance
// suite (benchmark LLD §12.24). The workspace HEAD is exported to a throwaway
// directory, the suite is copied into the target package, and go test runs
// INSIDE the agent image with no network: the code under test is
// agent-written and never runs on the host.
type podmanGrader struct {
	workspace string
	image     string
	// run is the seam for the podman call.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

var _ agentbench.AcceptanceGrader = podmanGrader{}

const (
	acceptanceTestTimeout  = "4m"            // go test's own bound: a hang is the code's
	acceptanceOuterTimeout = 5 * time.Minute // podman's bound: past it, a harness fault
	acceptanceOutputLimit  = 4096
)

func execCombined(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (g podmanGrader) Grade(ctx context.Context, spec agentbench.TaskSpec) agentbench.AcceptanceResult {
	suite, err := spec.AcceptanceDir()
	if err != nil || suite == "" || len(spec.Targets) != 1 {
		return acceptanceHarnessError(fmt.Errorf("task %q has no gradable suite or target: %v", spec.ID, err))
	}
	tmp, err := os.MkdirTemp("", "vornik-acceptance-")
	if err != nil {
		return acceptanceHarnessError(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	tree := filepath.Join(tmp, "tree")
	if err := exportHEAD(ctx, g.workspace, tree); err != nil {
		return acceptanceHarnessError(err)
	}
	pkg := filepath.Join(tree, filepath.Clean(spec.Targets[0]))
	if goFiles, _ := filepath.Glob(filepath.Join(pkg, "*.go")); len(goFiles) == 0 {
		return agentbench.AcceptanceResult{Outcome: agentbench.AcceptanceDoesNotCompile,
			Output: fmt.Sprintf("the package %s is missing at the workspace HEAD", spec.Targets[0])}
	}
	suiteFiles, _ := filepath.Glob(filepath.Join(suite, "*.go"))
	if len(suiteFiles) == 0 {
		return acceptanceHarnessError(fmt.Errorf("acceptance suite %s has no .go files", suite))
	}
	for _, f := range suiteFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return acceptanceHarnessError(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, filepath.Base(f)), data, 0o644); err != nil {
			return acceptanceHarnessError(err)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, acceptanceOuterTimeout)
	defer cancel()
	run := g.run
	if run == nil {
		run = execCombined
	}
	out, err := run(runCtx, "podman", "run", "--rm", "--network=none", "--pull=never", "--userns=keep-id",
		"--entrypoint", "go",
		"-v", tree+":/work:z", "-w", "/work/"+filepath.ToSlash(filepath.Clean(spec.Targets[0])),
		"-e", "GOFLAGS=-mod=mod", "-e", "GOPROXY=off", "-e", "GOWORK=off", "-e", "GOCACHE=/tmp/gocache",
		g.image, "test", "-count=1", "-timeout="+acceptanceTestTimeout,
		// Only the suite's tests run: the agent's own tests must compile, but
		// a failing one is not the independent grade.
		"-run=^TestAcceptance_", "./...")
	return classifyAcceptance(runCtx, string(out), err)
}

// classifyAcceptance maps go test's result onto the §12.24 outcomes.
func classifyAcceptance(ctx context.Context, out string, err error) agentbench.AcceptanceResult {
	res := agentbench.AcceptanceResult{Output: truncateAcceptance(out)}
	var exitErr *exec.ExitError
	code := -1
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	switch {
	case err == nil:
		return agentbench.AcceptanceResult{Outcome: agentbench.AcceptancePassed}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Outcome = agentbench.AcceptanceError // the OUTER bound: a harness fault
	case strings.Contains(out, "panic: test timed out"):
		res.Outcome = agentbench.AcceptanceTimeout
	case strings.Contains(out, "[build failed]") || strings.Contains(out, "[setup failed]"):
		res.Outcome = agentbench.AcceptanceDoesNotCompile
	case code == 125 || code == 126 || code == 127 || strings.Contains(err.Error(), "exit status 125") || strings.HasPrefix(strings.TrimSpace(out), "Error:"):
		res.Outcome = agentbench.AcceptanceError // podman itself failed
	case strings.Contains(out, "--- FAIL"):
		res.Outcome = agentbench.AcceptanceFailed
	default:
		// No failing test in the output: not something the model can be
		// charged with.
		res.Outcome = agentbench.AcceptanceError
	}
	return res
}

func acceptanceHarnessError(err error) agentbench.AcceptanceResult {
	return agentbench.AcceptanceResult{Outcome: agentbench.AcceptanceError, Output: truncateAcceptance(err.Error())}
}

func truncateAcceptance(s string) string {
	if len(s) <= acceptanceOutputLimit {
		return s
	}
	return s[:acceptanceOutputLimit] + "\n[truncated]"
}

// exportHEAD writes the workspace's committed HEAD (never the working tree)
// into dir. git and tar are harmless on the host; only the tests run sandboxed.
func exportHEAD(ctx context.Context, workspace, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tar := dir + ".tar"
	if out, err := exec.CommandContext(ctx, "git", "-C", workspace, "archive", "--format=tar", "-o", tar, "HEAD").CombinedOutput(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "tar", "-xf", tar, "-C", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %w: %s", err, out)
	}
	return os.Remove(tar)
}

// acceptanceLine is the rollup's descriptive acceptance line (§12.24): the
// denominator is every repeat, "graded" excludes not_graded, and harness
// errors are never counted as model failures.
func acceptanceLine(j agentbench.Journal) string {
	total, graded, passed, harness := 0, 0, 0, 0
	byOutcome := map[agentbench.AcceptanceOutcome]int{}
	for _, run := range j.TaskRuns {
		if run.Acceptance == nil {
			continue
		}
		total++
		byOutcome[run.Acceptance.Outcome]++
		switch run.Acceptance.Outcome {
		case agentbench.AcceptanceNotGraded:
		case agentbench.AcceptanceError:
			harness++
		default:
			graded++
			if run.Acceptance.Outcome == agentbench.AcceptancePassed {
				passed++
			}
		}
	}
	if total == 0 {
		return "not graded (no acceptance suites or no --acceptance-image; descriptive only, §12.24)"
	}
	return fmt.Sprintf("graded %d of %d, passed %d (failed %d, timeout %d, does not compile %d), harness errors %d (not graded); descriptive, n=%d",
		graded, total, passed, byOutcome[agentbench.AcceptanceFailed], byOutcome[agentbench.AcceptanceTimeout],
		byOutcome[agentbench.AcceptanceDoesNotCompile], harness, graded)
}

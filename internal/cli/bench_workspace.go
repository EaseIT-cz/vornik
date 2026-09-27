package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/agentbench"
)

// gitWorkspace clears a benchmark task's targets from the benchmark
// workspace before each repeat, so the task starts where its prompt says it
// does (benchmark LLD §12.23). It runs git, so it lives in vornikctl, the one
// binary allowed to spawn processes.
type gitWorkspace struct {
	dir string
}

var _ agentbench.WorkspacePreparer = gitWorkspace{}

func (w gitWorkspace) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", w.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Prepare removes every target present at HEAD in one commit and returns it,
// or "" when none was present. A workspace with uncommitted entries is
// refused: the daemon's merge would fail on it anyway, and a clear on top of
// unknown state proves nothing.
func (w gitWorkspace) Prepare(ctx context.Context, spec agentbench.TaskSpec, repeat int) (string, error) {
	status, err := w.git(ctx, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", fmt.Errorf("benchmark workspace %s has uncommitted entries; refusing to start %q on unknown state", w.dir, spec.ID)
	}
	var present []string
	for _, tg := range spec.Targets {
		if out, err := w.git(ctx, "ls-files", "--", tg); err == nil && out != "" {
			present = append(present, tg)
		}
	}
	if len(present) == 0 {
		return "", nil
	}
	if _, err := w.git(ctx, append([]string{"rm", "-r", "-q", "--"}, present...)...); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("bench: clear targets for %s r%d (§12.23)", spec.ID, repeat)
	if _, err := w.git(ctx, "commit", "-q", "-m", msg); err != nil {
		return "", err
	}
	for _, tg := range present {
		if _, err := os.Stat(filepath.Join(w.dir, tg)); !os.IsNotExist(err) {
			return "", fmt.Errorf("target %s still present after the clear commit", tg)
		}
	}
	return w.git(ctx, "rev-parse", "HEAD")
}

// Produced reports, per target, whether it is tracked at HEAD: the branch
// the daemon merges each task into.
func (w gitWorkspace) Produced(ctx context.Context, spec agentbench.TaskSpec) (map[string]bool, error) {
	out := make(map[string]bool, len(spec.Targets))
	for _, tg := range spec.Targets {
		files, err := w.git(ctx, "ls-files", "--", tg)
		if err != nil {
			return nil, err
		}
		out[tg] = files != ""
	}
	return out, nil
}

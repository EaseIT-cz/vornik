package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		return "", fmt.Errorf("benchmark workspace %s has uncommitted entries; refusing to start %q on unknown state:\n%s",
			w.dir, spec.ID, w.describeDirt(ctx))
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

// maxDirtListed caps how many dirty entries a refusal lists; the total is
// always stated.
const maxDirtListed = 20

// depositTaskRE matches the task id in a backlog deposit's trailer
// ("(via task_..., <date>)", api.renderBacklogDepositLine).
var depositTaskRE = regexp.MustCompile(`via (task_[0-9A-Za-z_]+)`)

// describeDirt lists the workspace's uncommitted entries and, per entry, the
// tasks named by "via task_..." trailers in its UNCOMMITTED lines: the diff
// against HEAD for a tracked path, the content of an untracked one. Best
// effort and diagnostic only (benchmark LLD §12.25 D4, GitHub #307): the
// refusal has already been decided, this only says what to look at.
func (w gitWorkspace) describeDirt(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "-C", w.dir, "status", "--porcelain", "-z", "-uall").Output()
	if err != nil {
		return "  (could not list the entries: " + err.Error() + ")"
	}
	var entries []string
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		entries = append(entries, f)
		if f[0] == 'R' || f[0] == 'C' {
			i++ // a rename's source path follows as its own field
		}
	}
	var b strings.Builder
	for i, e := range entries {
		if i == maxDirtListed {
			fmt.Fprintf(&b, "  ... and %d more (%d in all)\n", len(entries)-maxDirtListed, len(entries))
			break
		}
		code, path := e[:2], e[3:]
		fmt.Fprintf(&b, "  %s %s  origin: %s\n", code, path, w.dirtOrigin(ctx, code, path))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (w gitWorkspace) dirtOrigin(ctx context.Context, code, path string) string {
	var text []byte
	if code == "??" {
		text, _ = os.ReadFile(filepath.Join(w.dir, path)) //nolint:gosec // a path git listed inside the workspace
	} else {
		diff, _ := exec.CommandContext(ctx, "git", "-C", w.dir, "diff", "HEAD", "--", path).Output()
		var added []string
		for _, line := range strings.Split(string(diff), "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				added = append(added, line)
			}
		}
		text = []byte(strings.Join(added, "\n"))
	}
	seen := map[string]bool{}
	var tasks []string
	for _, m := range depositTaskRE.FindAllStringSubmatch(string(text), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			tasks = append(tasks, m[1])
		}
	}
	if len(tasks) == 0 {
		return "origin unknown"
	}
	return strings.Join(tasks, ", ") + " (backlog deposit trailer)"
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

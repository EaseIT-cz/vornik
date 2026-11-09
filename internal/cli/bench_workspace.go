package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/spawn"
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

// gitStdout runs git and returns its stdout only, for output that is parsed:
// git writes warnings to stderr (a broken alternates entry, for one), and
// those must not be read as data. stderr is kept for the error.
func (w gitWorkspace) gitStdout(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", w.dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// Prepare resets the workspace for one task repeat and returns the commit the
// task starts from, or "" when nothing needed resetting. It removes every
// declared target and the workflow's own state (agentbench.WorkflowStatePaths,
// benchmark LLD §12.23, §12.29). It then replaces the history with one root
// commit, after archiving every ref to a sibling bare repository (§12.30).
// The agent containers mount the project's .git, so any commit left there is
// readable from inside a step. A failed task's uncommitted leftovers are
// stashed first, so the reset archives them with the history and names them
// in the root commit (§12.31); the task starts from a pristine tree.
func (w gitWorkspace) Prepare(ctx context.Context, spec agentbench.TaskSpec, repeat int) (string, error) {
	// A fresh bench instance has no workspace until its first task runs, and
	// this runs before that: bootstrap it exactly as the daemon would (#593).
	// The executor's git runs under the spawn law, which allows only registered
	// roots: register the workspaces directory, as the daemon does.
	if err := spawn.RegisterWorkspaceRoot(filepath.Dir(w.dir)); err != nil {
		return "", err
	}
	if err := executor.EnsureGitRepo(ctx, w.dir, zerolog.Nop()); err != nil {
		return "", fmt.Errorf("bootstrap the benchmark workspace: %w", err)
	}
	gitDir, err := w.gitStdout(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	// Before status: git itself complains about a broken alternates entry,
	// and that complaint would read as uncommitted entries.
	if err := w.refuseUnresettable(ctx, gitDir); err != nil {
		return "", err
	}
	status, err := w.git(ctx, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	// A failed task's leftovers (#600, §12.31): stash them, untracked files
	// included, so the reset below archives them with the history as
	// refs/stash. The task then starts from a pristine tree, and the root
	// commit names what was archived. Ignored paths stay, as git leaves them.
	leftovers := ""
	if status != "" {
		leftovers = w.describeDirt(ctx)
		if _, err := w.git(ctx, "-c", "user.name=vornik-bench", "-c", "user.email=bench@vornik.io",
			"stash", "push", "-q", "--include-untracked", "-m",
			fmt.Sprintf("bench: leftovers before %s r%d (§12.31)", spec.ID, repeat)); err != nil {
			return "", fmt.Errorf("archive the leftovers in %s before %q: %w", w.dir, spec.ID, err)
		}
		if rest, err := w.git(ctx, "status", "--porcelain"); err != nil || rest != "" {
			return "", fmt.Errorf("benchmark workspace %s still has uncommitted entries after stashing the leftovers; refusing to start %q:\n%s",
				w.dir, spec.ID, rest)
		}
	}
	var present []string
	for _, tg := range append(append([]string(nil), spec.Targets...), agentbench.WorkflowStatePaths...) {
		if out, err := w.git(ctx, "ls-files", "--", tg); err == nil && out != "" {
			present = append(present, tg)
		}
	}
	single, err := w.historyIsOneCommit(ctx, gitDir)
	if err != nil {
		return "", err
	}
	if len(present) == 0 && single {
		return "", nil
	}
	if len(present) > 0 {
		if _, err := w.git(ctx, append([]string{"rm", "-r", "-q", "--"}, present...)...); err != nil {
			return "", err
		}
	}
	root, err := w.replaceHistory(ctx, gitDir, spec, repeat, leftovers)
	if err != nil {
		return "", err
	}
	for _, tg := range present {
		if _, err := os.Stat(filepath.Join(w.dir, tg)); !os.IsNotExist(err) {
			return "", fmt.Errorf("%s still present after the reset", tg)
		}
	}
	return root, nil
}

// specialHeads are the refs git keeps as files outside refs/. Each can name an
// old commit, and gc keeps what they name (§12.30).
var specialHeads = []string{"ORIG_HEAD", "FETCH_HEAD", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "AUTO_MERGE"}

// refuseUnresettable refuses a workspace whose history cannot be fully
// removed: a live worktree pins its branch, and objects reached through
// alternates are outside what gc removes. Records of worktrees whose
// directory is gone (a daemon that died mid-task) are pruned first.
func (w gitWorkspace) refuseUnresettable(ctx context.Context, gitDir string) error {
	if _, err := os.Stat(filepath.Join(gitDir, "objects", "info", "alternates")); err == nil {
		return fmt.Errorf("benchmark workspace %s uses objects/info/alternates; its history cannot be removed (§12.30)", w.dir)
	}
	if _, err := w.git(ctx, "worktree", "prune"); err != nil {
		return err
	}
	list, err := w.gitStdout(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	var others []string
	for i, line := range strings.Split(list, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok && i > 0 {
			others = append(others, path)
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("benchmark workspace %s has a live worktree, which keeps the earlier history; tasks run one at a time, so it is a task still running or a leftover to remove by hand (§12.30): %s",
			w.dir, strings.Join(others, ", "))
	}
	return nil
}

// historyIsOneCommit reports whether the workspace holds one commit, one ref
// and no special head: a reset would have nothing left to remove.
func (w gitWorkspace) historyIsOneCommit(ctx context.Context, gitDir string) (bool, error) {
	refs, err := w.gitStdout(ctx, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return false, err
	}
	if strings.Count(refs, "\n") > 0 {
		return false, nil
	}
	for _, h := range specialHeads {
		if _, err := os.Stat(filepath.Join(gitDir, h)); err == nil {
			return false, nil
		}
	}
	n, err := w.commitObjects(ctx)
	return n == 1, err
}

// commitObjects counts the commit objects in the whole object store, the
// check that covers every way an old commit could survive.
func (w gitWorkspace) commitObjects(ctx context.Context) (int, error) {
	out, err := w.gitStdout(ctx, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if l == "commit" {
			n++
		}
	}
	return n, nil
}

// replaceHistory archives every ref to <workspace>.history.git under
// refs/bench/<task>/r<repeat>/<UTC time>/, commits the index as a root commit on the
// current branch, removes every other ref, special head and reflog, prunes
// the old objects, and verifies one commit is left (§12.30).
func (w gitWorkspace) replaceHistory(ctx context.Context, gitDir string, spec agentbench.TaskSpec, repeat int, leftovers string) (string, error) {
	if err := w.archiveRefs(ctx, spec, repeat); err != nil {
		return "", err
	}
	branch, err := w.gitStdout(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	tree, err := w.gitStdout(ctx, "write-tree")
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("bench: reset workspace for %s r%d (§12.23, §12.29, §12.30)", spec.ID, repeat)
	if leftovers != "" {
		msg += "\n\nLeftovers archived as refs/stash (§12.31):\n" + leftovers
	}
	// A fixed identity, as the daemon commits as vornik-agent: the harness
	// runs under an env -i allowlist with no git identity, and "Author
	// identity unknown" stopped the arm (#660). --no-gpg-sign: a bench
	// workspace has no signing key, and a commit.gpgsign inherited from the
	// user's config would fail the reset.
	root, err := w.gitStdout(ctx, "-c", "user.name=vornik-bench", "-c", "user.email=bench@vornik.io",
		"commit-tree", "--no-gpg-sign", tree, "-m", msg)
	if err != nil {
		return "", err
	}
	if _, err := w.git(ctx, "update-ref", "refs/heads/"+branch, root); err != nil {
		return "", err
	}
	if err := w.dropOldHistory(ctx, gitDir, "refs/heads/"+branch); err != nil {
		return "", err
	}
	if n, err := w.commitObjects(ctx); err != nil || n != 1 {
		return "", fmt.Errorf("benchmark workspace %s holds %d commit objects after the reset, want 1 (§12.30): %v", w.dir, n, err)
	}
	if all, err := w.gitStdout(ctx, "rev-list", "--all"); err != nil || all != root {
		return "", fmt.Errorf("benchmark workspace %s: rev-list --all = %q after the reset, want only %s (§12.30): %v", w.dir, all, root, err)
	}
	return root, nil
}

// archiveRefs pushes every ref, by name, to <workspace>.history.git under
// refs/bench/<task>/r<repeat>/<UTC time>/ (§12.30). A "refs/*" glob refspec
// skips refs/stash, so each ref is named. Each attempt archives under its own
// timestamp and nothing is forced, so a retry never overwrites an earlier
// archive.
func (w gitWorkspace) archiveRefs(ctx context.Context, spec agentbench.TaskSpec, repeat int) error {
	archive := w.dir + ".history.git"
	if _, err := os.Stat(archive); os.IsNotExist(err) {
		if out, err := exec.CommandContext(ctx, "git", "init", "-q", "--bare", archive).CombinedOutput(); err != nil {
			return fmt.Errorf("create the history archive %s: %w: %s", archive, err, strings.TrimSpace(string(out)))
		}
	}
	all, err := w.gitStdout(ctx, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	ns := fmt.Sprintf("refs/bench/%s/r%d/%s/", spec.ID, repeat, time.Now().UTC().Format("20060102T150405.000000000Z"))
	push := []string{"push", "-q", archive}
	for _, ref := range strings.Split(all, "\n") {
		if ref != "" {
			push = append(push, ref+":"+ns+ref)
		}
	}
	if _, err := w.git(ctx, push...); err != nil {
		return fmt.Errorf("archive the workspace history: %w", err)
	}
	return nil
}

// dropOldHistory deletes every ref but keep, the special heads, and every
// reflog, then prunes what nothing reaches any more (§12.30).
func (w gitWorkspace) dropOldHistory(ctx context.Context, gitDir, keep string) error {
	refs, err := w.gitStdout(ctx, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	for _, ref := range strings.Split(refs, "\n") {
		if ref != "" && ref != keep {
			if _, err := w.git(ctx, "update-ref", "-d", ref); err != nil {
				return err
			}
		}
	}
	for _, h := range specialHeads {
		if err := os.Remove(filepath.Join(gitDir, h)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if _, err := w.git(ctx, "reflog", "expire", "--expire=now", "--all"); err != nil {
		return err
	}
	_, err = w.git(ctx, "gc", "-q", "--prune=now")
	return err
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

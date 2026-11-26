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
	// taskStatus asks the bench daemon for a task's status (#915, §12.30
	// amendment 2026-11-25): found=false is a 404, err a failed read. Nil
	// (vornikctl bench bare) keeps the immediate refusal of a live worktree.
	taskStatus func(ctx context.Context, taskID string) (status string, found bool, err error)
	// waitBound and pollEvery bound the wait for a live task's worktree;
	// zero means replayWaitBound and replayPollEvery.
	waitBound, pollEvery time.Duration
	// afterWait is a test seam: it runs once the wait found no live worktree,
	// before the reset, so a test can create one there.
	afterWait func()
}

// replayWaitBound is one healing replay trial's bound
// (workflowhealing.DefaultAsyncTrialBound); replayPollEvery the poll.
const (
	replayWaitBound = 45 * time.Minute
	replayPollEvery = 15 * time.Second
)

// liveTaskStatuses are the statuses of a task whose worktree is still in use;
// waiting for it is right. COMPLETED is not one: the executor removes a
// task's worktree before it marks the task COMPLETED, so a COMPLETED task's
// worktree is a leftover (§12.30 amendment 2026-11-25).
var liveTaskStatuses = map[string]bool{
	"PENDING": true, "QUEUED": true, "LEASED": true, "RUNNING": true,
	"WAITING_FOR_CHILDREN": true, "AWAITING_INPUT": true, "AWAITING_EXTERNAL": true,
	"AWAITING_APPROVAL": true, "PAUSED": true,
}

// terminalTaskStatuses are a leftover's: refused at once.
var terminalTaskStatuses = map[string]bool{"COMPLETED": true, "FAILED": true, "CANCELLED": true, "CLOSED": true}

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
// task starts from, or "" when nothing needed resetting. Every repeat starts
// from the harness's baseline files plus the task's seed (benchmark LLD
// §12.33): every other tracked path is removed, whatever its name, and the
// baseline files are written with the harness's content. It then replaces
// the history with one root commit, after archiving every ref to a sibling
// bare repository (§12.30). The agent containers mount the project's .git,
// so any commit left there is readable from inside a step. A failed task's
// uncommitted leftovers are stashed first, so the reset archives them with
// the history and names them in the root commit (§12.31).
func (w gitWorkspace) Prepare(ctx context.Context, spec agentbench.TaskSpec, repeat int) (string, error) {
	// A fresh bench instance has no workspace until its first task runs, and
	// this runs before that: bootstrap it exactly as the daemon would (#593).
	// Every path below is used from inside `git -C <dir>` (the history archive
	// is <dir>.history.git): an absolute dir keeps a relative --workspace from
	// resolving them against the workspace itself (#915).
	if abs, err := filepath.Abs(w.dir); err == nil {
		w.dir = abs
	}
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
	if err := w.refuseAlternates(gitDir); err != nil {
		return "", err
	}
	if err := w.awaitWorktrees(ctx); err != nil {
		return "", err
	}
	if w.afterWait != nil {
		w.afterWait()
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
	removed, err := w.removeAllButBaseline(ctx)
	if err != nil {
		return "", err
	}
	rewrote, err := w.writeBaseline(ctx)
	if err != nil {
		return "", err
	}
	single, err := w.historyIsOneCommit(ctx, gitDir)
	if err != nil {
		return "", err
	}
	seed, err := spec.SeedDir()
	if err != nil {
		return "", err
	}
	// A seeded task always resets, so each repeat starts from a root commit
	// holding exactly its seed (GitHub #650). Stashed leftovers force a reset
	// too: the stash must reach the archive and leave the workspace, where a
	// step could read it (review-20261118-a85d F6, §12.31).
	if len(removed) == 0 && !rewrote && single && seed == "" && leftovers == "" {
		return "", nil
	}
	if seed != "" {
		if err := w.applySeed(ctx, spec, seed); err != nil {
			return "", err
		}
	}
	return w.replaceHistory(ctx, gitDir, spec, repeat, leftovers)
}

// baselineFiles are the files every repeat starts from and their content,
// which the harness owns (§12.33).
func baselineFiles() map[string]string {
	return map[string]string{
		agentbench.BaselineGoModPath:  agentbench.BaselineGoMod,
		agentbench.BaselineMarkerPath: executor.BootstrapMarkerBody,
	}
}

// removeAllButBaseline removes every tracked path that is not a baseline
// file and checks each is gone. The removed content stays reachable from the
// refs archiveRefs keeps (§12.30). The check runs BEFORE any seed goes back
// in, so a seeded file is never mistaken for a leftover (review-20261116-0c7d
// H2).
func (w gitWorkspace) removeAllButBaseline(ctx context.Context) ([]string, error) {
	out, err := w.gitStdout(ctx, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	keep := baselineFiles()
	var removed []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" && keep[f] == "" {
			removed = append(removed, f)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	// git rm removes the files and the directories it leaves empty.
	if _, err := w.git(ctx, append([]string{"rm", "-r", "-q", "--"}, removed...)...); err != nil {
		return nil, err
	}
	for _, f := range removed {
		if _, err := os.Lstat(filepath.Join(w.dir, f)); !os.IsNotExist(err) {
			return nil, fmt.Errorf("%s still present after the reset", f)
		}
	}
	return removed, nil
}

// writeBaseline writes each baseline file with the harness's content and
// stages it; it reports whether any file differed from what was tracked.
func (w gitWorkspace) writeBaseline(ctx context.Context) (bool, error) {
	changed := false
	for rel, content := range baselineFiles() {
		full := filepath.Join(w.dir, rel)
		if cur, err := os.ReadFile(full); err == nil && string(cur) == content {
			if tracked, err := w.git(ctx, "ls-files", "--", rel); err == nil && tracked != "" {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return false, err
		}
		if _, err := w.git(ctx, "add", "--", rel); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// applySeed copies the seed's files into the task's single target and stages
// exactly those files, so the root commit replaceHistory writes holds them
// (benchmark design, Amendment 2026-11-16, GitHub #650).
func (w gitWorkspace) applySeed(ctx context.Context, spec agentbench.TaskSpec, seed string) error {
	if len(spec.Targets) != 1 {
		return fmt.Errorf("task %q has a seed, so it must declare exactly one target (it declares %d)", spec.ID, len(spec.Targets))
	}
	files, err := agentbench.SeedFiles(seed)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("task %q: seed %s holds no files", spec.ID, seed)
	}
	target := filepath.Clean(spec.Targets[0])
	staged := make([]string, 0, len(files))
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(seed, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("read seed of %q: %w", spec.ID, err)
		}
		dst := filepath.Join(w.dir, target, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		staged = append(staged, filepath.ToSlash(filepath.Join(target, filepath.FromSlash(rel))))
	}
	if _, err := w.git(ctx, append([]string{"add", "-f", "--"}, staged...)...); err != nil {
		return fmt.Errorf("stage the seed of %q: %w", spec.ID, err)
	}
	return nil
}

// specialHeads are the refs git keeps as files outside refs/. Each can name an
// old commit, and gc keeps what they name (§12.30).
var specialHeads = []string{"ORIG_HEAD", "FETCH_HEAD", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "AUTO_MERGE"}

// The reset refuses a workspace whose history cannot be fully removed: objects
// reached through alternates are outside what gc removes (refuseAlternates),
// and a live worktree pins its branch (awaitWorktrees, refuseLiveWorktrees).
// Records of worktrees whose directory is gone (a daemon that died mid-task)
// are pruned first (liveWorktrees).

// refuseAlternates refuses a workspace whose objects come through alternates.
func (w gitWorkspace) refuseAlternates(gitDir string) error {
	if _, err := os.Stat(filepath.Join(gitDir, "objects", "info", "alternates")); err == nil {
		return fmt.Errorf("benchmark workspace %s uses objects/info/alternates; its history cannot be removed (§12.30)", w.dir)
	}
	return nil
}

// liveWorktrees prunes the records of worktrees whose directory is gone and
// lists the rest, the main worktree excluded.
func (w gitWorkspace) liveWorktrees(ctx context.Context) ([]string, error) {
	// A replay's own cleanup can remove the last worktree while prune runs;
	// git then deletes .git/worktrees under it and prune fails ("Invalid
	// path"). Retried briefly: the race ends when the other remove does
	// (benchmark LLD §12.30, as shipped; gate flake 2026-11-25).
	if err := retryGitRace(ctx, 3, 100*time.Millisecond, func() error {
		_, err := w.git(ctx, "worktree", "prune")
		return err
	}); err != nil {
		return nil, err
	}
	list, err := w.gitStdout(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var others []string
	for i, line := range strings.Split(list, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok && i > 0 {
			others = append(others, path)
		}
	}
	return others, nil
}

// resolvedPath is p made absolute with its symlinks resolved, or p as given
// when it cannot be (the comparison then fails closed: a refusal).
func resolvedPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// refuseLiveWorktrees refuses, without waiting, when any worktree is live.
// The reset calls it immediately before it deletes refs and before gc, so a
// replay that created its worktree after the wait keeps its branch (#915).
func (w gitWorkspace) refuseLiveWorktrees(ctx context.Context) error {
	others, err := w.liveWorktrees(ctx)
	if err != nil {
		return err
	}
	if len(others) > 0 {
		return fmt.Errorf("benchmark workspace %s has a live worktree, which keeps the earlier history; tasks run one at a time, so it is a task still running or a leftover to remove by hand (§12.30): %s",
			w.dir, strings.Join(others, ", "))
	}
	return nil
}

// awaitWorktrees waits, bounded, while every live worktree belongs to a task
// the daemon reports live (a learning arm's healing replay, #915), and refuses
// at once for a leftover: a terminal or unknown task, or a worktree that is
// not <workspace>/.worktrees/<task id>. A failed status read is retried at
// the next poll. Without a status lookup it refuses as before.
func (w gitWorkspace) awaitWorktrees(ctx context.Context) error {
	if w.taskStatus == nil {
		return w.refuseLiveWorktrees(ctx)
	}
	bound, every := w.waitBound, w.pollEvery
	if bound <= 0 {
		bound = replayWaitBound
	}
	if every <= 0 {
		every = replayPollEvery
	}
	deadline := time.Now().Add(bound)
	// git lists worktrees by absolute path; resolve both sides so a relative
	// or symlinked --workspace still recognises its task worktrees.
	wtRoot := resolvedPath(filepath.Join(w.dir, ".worktrees"))
	for {
		others, err := w.liveWorktrees(ctx)
		if err != nil {
			return err
		}
		if len(others) == 0 {
			return nil
		}
		var waiting []string
		for _, path := range others {
			id := filepath.Base(path)
			if resolvedPath(filepath.Dir(path)) != wtRoot || !strings.HasPrefix(id, "task_") {
				return fmt.Errorf("benchmark workspace %s has a worktree that is not a task worktree (%s/<task id>); someone added it by hand, remove it (§12.30): %s", w.dir, wtRoot, path)
			}
			status, found, lerr := w.taskStatus(ctx, id)
			switch {
			case lerr != nil:
				waiting = append(waiting, id+" (status unreadable: "+lerr.Error()+")")
			case !found:
				return fmt.Errorf("benchmark workspace %s has a live worktree of a task the daemon does not know, a leftover to remove by hand (§12.30): %s", w.dir, path)
			case terminalTaskStatuses[status]:
				return fmt.Errorf("benchmark workspace %s has a live worktree of a %s task, a leftover to remove by hand (§12.30): %s", w.dir, status, path)
			case liveTaskStatuses[status]:
				waiting = append(waiting, id+" ("+status+")")
			default:
				waiting = append(waiting, id+" (status "+status+" not recognised)")
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("benchmark workspace %s: tasks still running after %s, holding a worktree (a learning arm's replay?): %s; re-run the chunk once they finish (§12.30)",
				w.dir, bound, strings.Join(waiting, ", "))
		}
		_, _ = fmt.Fprintf(os.Stderr, "  ..  waiting for %s to release the benchmark workspace\n", strings.Join(waiting, ", "))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
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
	msg := fmt.Sprintf("bench: reset workspace for %s r%d (§12.23, §12.29, §12.30, §12.33)", spec.ID, repeat)
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
	// A replay may have created a worktree since the wait (#915): never delete
	// its branch or prune its objects.
	if err := w.refuseLiveWorktrees(ctx); err != nil {
		return err
	}
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
	if err := w.refuseLiveWorktrees(ctx); err != nil {
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

// retryGitRace runs fn up to attempts times, pausing between tries, and
// returns its last error. For a git call that races another git process on
// the same repository's administrative files.
func retryGitRace(ctx context.Context, attempts int, pause time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i+1 < attempts {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(pause):
			}
		}
	}
	return err
}

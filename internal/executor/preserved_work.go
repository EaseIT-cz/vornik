package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// preservedWorkRefPrefix is where a failed merge-back's work is pinned
// (executor design §9.5.2). Outside refs/heads, so `git branch -D`,
// removeWorktree, pruneWorktrees and doctor's host cleanup never delete it,
// and gc keeps its objects. Sibling of backupLocalOnlyCommits'
// refs/vornik/discarded/.
const preservedWorkRefPrefix = "refs/vornik/preserved/"

// PreservedWorkRetention bounds how long a pinned ref is kept: startup
// pruneWorktrees deletes refs whose name's timestamp is older (design §9.5.2
// P4). The doctor's preserved_work check lists every ref until then.
const PreservedWorkRetention = 30 * 24 * time.Hour

// PreservedWork is one pinned ref: the work of a task attempt whose
// merge-back failed.
type PreservedWork struct {
	Ref    string
	SHA    string
	TaskID string
	// Attempt and PreservedAt are parsed from the ref name; zero when the
	// name does not follow <task>/<attempt>-<unix seconds>-<short commit>.
	Attempt     int
	PreservedAt time.Time
}

// preservedTaskIDPattern bounds what goes into a ref name. Task ids are
// server-generated (task_<ts>_<hex>); anything else is refused rather than
// escaped.
var preservedTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

var preservedRefName = regexp.MustCompile(`^refs/vornik/preserved/([^/]+)/([0-9]+)-([0-9]+)(?:-[0-9a-f]+)?$`)

// preserveFailedWork pins the work a failed merge-back left in the task's
// worktree to refs/vornik/preserved/<task>/<attempt>-<unix seconds>-<short commit>.
//
// GitHub #354: the failure message promised "worktree preserved for manual
// recovery", and the task's own retry then removed the worktree and its
// branch. mergeWorktree has already auto-committed the worktree's leftovers,
// so the commit to pin is the worktree's HEAD, read through the daemon's own
// admin dir (S6-D2); it holds even when the worktree/<task> branch is gone,
// which is how all 8 prod merge-back failures in the 30 days to 2026-10-12
// failed ("not something we can merge"). Failing that, the branch tip.
//
// Returns a zero PreservedWork and nil when the commit is already reachable
// from the project's HEAD: there is nothing to recover. Returns an error,
// and pins nothing, when no commit can be found or the ref cannot be written.
func preserveFailedWork(ctx context.Context, projectDir, worktreeDir, taskID string, attempt int, now time.Time, logger zerolog.Logger) (PreservedWork, error) {
	if !preservedTaskIDPattern.MatchString(taskID) {
		return PreservedWork{}, fmt.Errorf("task id %q is not usable in a ref name", taskID)
	}
	sha := ""
	if worktreeDir != "" {
		if fi, err := os.Stat(worktreeDir); err == nil && fi.IsDir() {
			sha = revParseCommit(ctx, worktreeDir, "HEAD")
		}
	}
	if sha == "" {
		sha = revParseCommit(ctx, projectDir, "refs/heads/"+worktreeBranch(taskID))
	}
	if sha == "" {
		return PreservedWork{}, fmt.Errorf("neither the worktree's HEAD nor branch %s names a commit", worktreeBranch(taskID))
	}
	out, err := gitExec.output(ctx, "-C", projectDir, "rev-list", "--count", "HEAD.."+sha)
	if err != nil {
		return PreservedWork{}, fmt.Errorf("count the commits not on the project's HEAD: %w", err)
	}
	if strings.TrimSpace(string(out)) == "0" {
		return PreservedWork{}, nil
	}
	// The short commit id makes the name unique per commit, so two failures
	// of one attempt in one second (an operator retry that spends no
	// attempt) cannot collide on different work (review-20261012-4a89 F2).
	ref := fmt.Sprintf("%s%s/%d-%d-%s", preservedWorkRefPrefix, taskID, attempt, now.Unix(), shortSHA(sha))
	// Create-only: an empty old value makes update-ref refuse an existing ref.
	// A refusal for a ref that already names this commit is the same pin.
	if out, err := gitExec.combined(ctx, "-C", projectDir, "update-ref", ref, sha, ""); err != nil &&
		revParseCommit(ctx, projectDir, ref) != sha {
		logger.Error().Err(err).Str("task_id", taskID).Str("ref", ref).Str("commit", sha).
			Msg("merge-back failed and the work could NOT be pinned; it survives only in the worktree until the task is retried")
		return PreservedWork{}, fmt.Errorf("git update-ref %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	logger.Warn().Str("task_id", taskID).Str("ref", ref).Str("commit", sha).
		Msg("merge-back failed: the task's work is pinned to a preserved ref for recovery")
	return PreservedWork{Ref: ref, SHA: sha, TaskID: taskID, Attempt: attempt, PreservedAt: time.Unix(now.Unix(), 0)}, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// revParseCommit resolves rev to a commit id in dir, or "" when it does not.
// For a task worktree, gitExec names the daemon-derived admin dir as GIT_DIR
// (gitWorkspaceCmd → worktreeGitDirs, S6-D2), so the agent-writable .git
// pointer file is never read.
func revParseCommit(ctx context.Context, dir, rev string) string {
	out, err := gitExec.output(ctx, "-C", dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// prunePreservedWork deletes the project's preserved refs whose name's
// timestamp is older than retention (design §9.5.2 P4). A ref whose name does
// not parse is kept: nothing says how old it is. Best-effort, like the rest of
// the startup prune.
func prunePreservedWork(ctx context.Context, projectDir string, now time.Time, retention time.Duration, logger zerolog.Logger) {
	refs, err := ListPreservedWork(projectDir)
	if err != nil {
		logger.Warn().Err(err).Str("project_dir", projectDir).Msg("preserved work: could not list refs for retention")
		return
	}
	for _, p := range refs {
		if p.PreservedAt.IsZero() || now.Sub(p.PreservedAt) <= retention {
			continue
		}
		if out, err := gitExec.combined(ctx, "-C", projectDir, "update-ref", "-d", p.Ref, p.SHA); err != nil {
			logger.Warn().Err(err).Str("ref", p.Ref).Str("output", strings.TrimSpace(string(out))).
				Msg("preserved work: could not delete an expired ref")
			continue
		}
		logger.Info().Str("project_dir", projectDir).Str("ref", p.Ref).Str("commit", p.SHA).
			Dur("retention", retention).Msg("preserved work: deleted a ref past its retention")
	}
}

// ListPreservedWork lists projectDir's preserved refs by reading the
// repository's files: loose refs under .git/refs/vornik/preserved/ and
// .git/packed-refs. It runs no git, so the doctor can call it from a REST
// handler (process-spawn law S2). A loose ref overrides a packed one.
func ListPreservedWork(projectDir string) ([]PreservedWork, error) {
	gitDir := filepath.Join(projectDir, ".git")
	found := map[string]string{}
	if err := readPackedPreservedRefs(gitDir, found); err != nil {
		return nil, err
	}
	if err := readLoosePreservedRefs(gitDir, found); err != nil {
		return nil, err
	}
	out := make([]PreservedWork, 0, len(found))
	for name, sha := range found {
		out = append(out, parsePreservedRef(name, sha))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// readPackedPreservedRefs adds the preserved refs listed in packed-refs
// (git pack-refs, which gc runs, moves loose refs there).
func readPackedPreservedRefs(gitDir string, found map[string]string) error {
	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open packed-refs: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue // header, peeled-tag line
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && strings.HasPrefix(name, preservedWorkRefPrefix) {
			found[name] = sha
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read packed-refs: %w", err)
	}
	return nil
}

// readLoosePreservedRefs adds the loose refs under refs/vornik/preserved/,
// overriding a packed entry of the same name as git does.
func readLoosePreservedRefs(gitDir string, found map[string]string) error {
	looseRoot := filepath.Join(gitDir, filepath.FromSlash(strings.TrimSuffix(preservedWorkRefPrefix, "/")))
	err := filepath.WalkDir(looseRoot, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(gitDir, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if sha := strings.TrimSpace(string(raw)); sha != "" && !strings.HasPrefix(sha, "ref:") {
			found[filepath.ToSlash(rel)] = sha
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read loose preserved refs: %w", err)
	}
	return nil
}

// parsePreservedRef fills TaskID, and Attempt and PreservedAt when the name
// follows <task>/<attempt>-<unix seconds>-<short commit>.
func parsePreservedRef(name, sha string) PreservedWork {
	p := PreservedWork{Ref: name, SHA: sha}
	p.TaskID, _, _ = strings.Cut(strings.TrimPrefix(name, preservedWorkRefPrefix), "/")
	if m := preservedRefName.FindStringSubmatch(name); m != nil {
		p.Attempt, _ = strconv.Atoi(m[2])
		if ts, err := strconv.ParseInt(m[3], 10, 64); err == nil {
			p.PreservedAt = time.Unix(ts, 0)
		}
	}
	return p
}

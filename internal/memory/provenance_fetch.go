package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The daemon-only provenance clone's fetch (GitHub #499, operator decision
// A18; design 22, "Amendment 2026-10-21 (#482)", part 8g). Before each sweep
// the sweeper fetches the trusted branch into every configured clone, so a
// design merged on origin is verified without anyone touching the clone. A
// workspace clone (no memory.provenance_clone) is never fetched here.

// DefaultProvenanceFetchTimeout bounds one clone's fetch.
const DefaultProvenanceFetchTimeout = 90 * time.Second

// ProvenanceClone is one configured daemon-only clone.
type ProvenanceClone struct {
	ProjectID string
	Dir       string
	// RepoScope is the project's repo_scope: the clone's origin is checked
	// against it before every fetch, not only at load (review 05cc F2).
	RepoScope string
}

// Fetch outcomes, the metric's outcome label.
const (
	ProvenanceFetchOK       = "ok"
	ProvenanceFetchNoClone  = "no_clone"
	ProvenanceFetchNoBranch = "no_branch"
	ProvenanceFetchTimeout  = "timeout"
	ProvenanceFetchFailed   = "failed"
	// ProvenanceFetchRefused: the clone failed the verifier's own clone
	// checks (unsafe, no origin, origin is not repo_scope); nothing fetched.
	ProvenanceFetchRefused = "refused"
)

// ProvenanceFetchResult is a clone's last fetch, for the doctor.
type ProvenanceFetchResult struct {
	ProjectID string
	Dir       string
	Branch    string
	Outcome   string
	Error     string
	At        time.Time
}

// provenanceFetchState is the sweeper's record of the last fetch per project.
type provenanceFetchState struct {
	mu   sync.Mutex
	last map[string]ProvenanceFetchResult
}

// FetchClones fetches the trusted branch into every configured clone.
func (s *ProvenanceSweeper) FetchClones(ctx context.Context) {
	if s == nil || s.Clones == nil || s.Verifier == nil {
		return
	}
	for _, c := range s.Clones() {
		if ctx.Err() != nil {
			return
		}
		res := s.fetchClone(ctx, c)
		s.fetchState.mu.Lock()
		if s.fetchState.last == nil {
			s.fetchState.last = map[string]ProvenanceFetchResult{}
		}
		// An automatic clone has no project (#506 C1): it is keyed by scope.
		key := c.ProjectID
		if key == "" {
			key = "scope:" + c.RepoScope
		}
		s.fetchState.last[key] = res
		s.fetchState.mu.Unlock()
		if s.Metrics != nil && s.Metrics.ProvenanceFetchTotal != nil {
			s.Metrics.ProvenanceFetchTotal.WithLabelValues(c.ProjectID, res.Outcome).Inc()
		}
		ev := s.Logger.Debug()
		if res.Outcome != ProvenanceFetchOK {
			ev = s.Logger.Warn()
		}
		ev.Str("project_id", c.ProjectID).Str("dir", c.Dir).Str("branch", res.Branch).
			Str("outcome", res.Outcome).Str("error", res.Error).Msg("provenance sweep: clone fetch")
	}
}

func (s *ProvenanceSweeper) fetchClone(ctx context.Context, c ProvenanceClone) ProvenanceFetchResult {
	res := ProvenanceFetchResult{ProjectID: c.ProjectID, Dir: c.Dir, At: s.Verifier.now()}
	if c.Dir == "" {
		res.Outcome = ProvenanceFetchNoClone
		return res
	}
	// The clone is resolved ONCE and passes the verifier's own clone checks
	// (unbent, origin = repo_scope) before anything is fetched into it, so a
	// repointed origin is never fetched from (review 05cc F2, F3).
	dir, _, reason := s.Verifier.checkClone(ctx, c.ProjectID, c.RepoScope)
	switch {
	case reason == ProvReasonNoClone:
		res.Outcome = ProvenanceFetchNoClone
		return res
	case reason != "":
		res.Outcome, res.Error = ProvenanceFetchRefused, reason
		return res
	}
	branch, ok := s.Verifier.trustedBranch(ctx, dir, c.ProjectID, c.RepoScope)
	if !ok {
		res.Outcome = ProvenanceFetchNoBranch
		return res
	}
	res.Branch = branch
	timeout := s.FetchTimeout
	if timeout <= 0 {
		timeout = DefaultProvenanceFetchTimeout
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := s.Verifier.Git(fctx, dir, "fetch", "--quiet", "origin",
		"+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	switch {
	case err == nil:
		res.Outcome = ProvenanceFetchOK
	case errors.Is(fctx.Err(), context.DeadlineExceeded):
		res.Outcome, res.Error = ProvenanceFetchTimeout, err.Error()
	default:
		res.Outcome, res.Error = ProvenanceFetchFailed, err.Error()
	}
	return res
}

// LastFetches returns each configured clone's last fetch, by project.
func (s *ProvenanceSweeper) LastFetches() []ProvenanceFetchResult {
	if s == nil {
		return nil
	}
	s.fetchState.mu.Lock()
	defer s.fetchState.mu.Unlock()
	out := make([]ProvenanceFetchResult, 0, len(s.fetchState.last))
	for _, r := range s.fetchState.last {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out
}

// DaemonCloneDir resolves a configured memory.provenance_clone (design 22,
// part 8g). The clone is daemon-only: a path that resolves (symlinks
// followed on both sides) into, onto or above the agent-mounted workspace
// root is refused, because an agent could then write the objects the
// verifier reads. "" = refused or unresolvable, which the verifier reports
// as no clone (unverified), never as the workspace clone. The caller
// registers an accepted directory with the process-spawn law.
func DaemonCloneDir(clone, workspaceRoot string) string {
	if clone == "" || !filepath.IsAbs(clone) {
		return ""
	}
	dir, err := filepath.EvalSymlinks(clone)
	if err != nil {
		return ""
	}
	if !daemonOnlyWritable(dir) {
		return ""
	}
	if workspaceRoot != "" {
		root := filepath.Clean(workspaceRoot)
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		sep := string(filepath.Separator)
		if dir == root || strings.HasPrefix(dir, root+sep) || strings.HasPrefix(root, dir+sep) {
			return ""
		}
	}
	return dir
}

// daemonOnlyWritable reports whether the clone's directory, its .git, the
// object store, the refs and the config are each owned by the daemon's
// user and writable by no group or other (review 05cc F1). It is the check
// the daemon can make; the boundary against agents is that no agent mount
// reaches the clone (DaemonCloneDir's workspace-root refusal): a rootless
// container's root maps to the daemon's own uid, so ownership alone cannot
// tell an agent from the daemon. Deeper files are not walked.
func daemonOnlyWritable(dir string) bool {
	for _, p := range []string{dir, filepath.Join(dir, ".git"), filepath.Join(dir, ".git", "objects"),
		filepath.Join(dir, ".git", "refs"), filepath.Join(dir, ".git", "config")} {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o022 != 0 || !ownedByDaemon(fi) {
			return false
		}
	}
	return true
}

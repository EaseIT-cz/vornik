package memory

// Automatic daemon-only provenance clones, one per repository scope (GitHub
// #506; design 22, "Amendment 2026-10-22 (#506)", C1 and C2). Out of the box
// a project has no clone, and a companion project serves many repositories,
// so the daemon clones each repository the first time a companion document
// ingest names it, and fetches it before verifying. The clone holds commits
// and trees only (--filter=blob:none): the verifier compares blob ids read
// from trees and never needs a blob.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vornik.io/vornik/internal/spawn"
)

// Defaults of the automatic clone limits (design 22 #506 C1, review 28a5 F5).
const (
	DefaultAutoCloneMax        = 100
	DefaultAutoCloneMaxBytes   = int64(512 << 20)
	DefaultAutoCloneTotalBytes = int64(5 << 30)
	DefaultAutoCloneFailMemory = time.Hour
	DefaultAutoCloneFetchAfter = 2 * time.Minute
	autoCloneCreateSlots       = 2
	autoCloneFetchSlots        = 4
)

// DefaultAutoCloneHosts are the forges an automatic clone may be made from,
// besides the host of any configured forge integration.
var DefaultAutoCloneHosts = []string{"github.com", "gitlab.com", "bitbucket.org", "codeberg.org"}

// CloneRunner runs git for the automatic clones. The production runner goes
// through the process-spawn law's provenance kind.
type CloneRunner interface {
	Clone(ctx context.Context, root, url, dest, authHeader string) error
	Fetch(ctx context.Context, dir, branch, authHeader string) error
	OriginHead(ctx context.Context, dir string) (string, error)
}

// AutoClones manages the automatic clones under Root, laid out as
// <Root>/<host>/<owner>/<repo>.
type AutoClones struct {
	Root string
	// Hosts replaces DefaultAutoCloneHosts when set (tests); the daemon
	// leaves it empty and lists forges in Forges (#508 F1).
	Hosts []string
	// Forges are the configured self-hosted forges (memory.provenance.forges).
	Forges []ProvenanceForge
	// ReplaceDefaultHosts drops DefaultAutoCloneHosts (air-gapped installs).
	ReplaceDefaultHosts bool
	// AppToken returns a minted GitHub App installation token and its
	// installation id for a GitHub repository owner, from the REQUESTING
	// project's own github: block only ("" when it has none for that owner;
	// #508 F3).
	// identity names the whole App (API host, App id, installation), which
	// the clone's marker records: an installation id alone is configuration
	// any project can name (security review of 180ed1da9).
	AppToken func(ctx context.Context, projectID, host, owner string) (token, identity string)
	// AppTokenByIdentity mints a token for the App identity the sweep finds
	// recorded on a clone (SweepFetch). Nil = fetch anonymously.
	AppTokenByIdentity func(ctx context.Context, identity string) string
	Run                CloneRunner
	Now                func() time.Time

	MaxClones     int
	MaxCloneBytes int64
	MaxTotalBytes int64
	FailMemory    time.Duration
	FetchAfter    time.Duration

	once      sync.Once
	mu        sync.Mutex // guards the maps and the count/size check with the create
	failed    map[string]time.Time
	unmade    map[string]time.Time // creations that failed or were refused by a limit (#508 F4)
	refusals  []ProvenanceRefusal  // recent refusals, for the doctor (#508 F6)
	fetched   map[string]time.Time
	scopeMu   map[string]*sync.Mutex
	pending   int // creations reserved but not finished, counted against MaxClones
	createSem chan struct{}
	fetchSem  chan struct{}
}

func (a *AutoClones) init() {
	a.once.Do(func() {
		a.failed = map[string]time.Time{}
		a.unmade = map[string]time.Time{}
		a.fetched = map[string]time.Time{}
		a.scopeMu = map[string]*sync.Mutex{}
		a.createSem = make(chan struct{}, autoCloneCreateSlots)
		a.fetchSem = make(chan struct{}, autoCloneFetchSlots)
	})
}

func (a *AutoClones) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func orDefault[T int | int64 | time.Duration](v, d T) T {
	if v > 0 {
		return v
	}
	return d
}

// autoCloneSegment is one path segment of a scope: host, owner or repo.
var autoCloneSegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// isClone reports a finished automatic clone: a .git directory and the
// last-used marker, which is written only after the clone was renamed into
// place, so a clone still being made is never read (code review 5420 F3).
func isClone(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil || !fi.IsDir() {
		return false
	}
	_, err = os.Lstat(filepath.Join(dir, ".git", lastUsedMarker))
	return err == nil
}

// Ensure returns the scope's clone, creating it on first use and fetching
// it when its last fetch is older than FetchAfter. reason is "" when the
// clone is usable, else ProvReasonCloneFailed: the scope is not a listed
// repository, a limit refused it, or the clone failed (remembered for
// FailMemory so a repository without access costs one attempt an hour).
//
// projectID is the project whose ingest asks (#508 F2): a clone made with a
// credential is returned only to a project allowed that credential, checked
// on every call; a clone with no credential marker is made again.
func (a *AutoClones) Ensure(ctx context.Context, projectID, scope string) (string, string) {
	a.init()
	t, reason := a.resolve(scope)
	if reason != "" {
		a.refuse(projectID, scope, reason, resolveFix(reason))
		return "", reason
	}
	dir, ok := a.dirForTarget(t)
	if !ok {
		return "", ProvReasonCloneFailed
	}
	key := t.key
	lock := a.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	if isClone(dir) {
		marker, ok := readCredentialMarker(dir)
		if ok && staleForgeMarker(marker, t) {
			ok = false // the forge that made it is gone or renamed: make it again
		}
		if !ok && sweepCaller(ctx) {
			// The sweep never re-makes a clone; the next project use does.
			return "", ProvReasonNoClone
		}
		if ok {
			header, allowed := a.mayUse(ctx, projectID, marker, t)
			if sweepCaller(ctx) {
				// The daemon's own sweep reads and fetches; it compares no
				// content, so it is no oracle (SweepFetch).
				allowed = true
			}
			if !allowed {
				a.refuse(projectID, scope, "project "+projectID+" may not use this clone's credential ("+marker+")",
					"add "+projectID+" to the forge's allowed_projects, or give the project its own GitHub App block for this owner")
				return "", ProvReasonCloneFailed
			}
			touchLastUsed(dir)
			if !sweepCaller(ctx) {
				a.freshen(ctx, key, dir, header)
			}
			return dir, ""
		}
		// No marker: made before #508, or tampered. Fail closed by making it
		// again with this project's credential.
		_ = os.RemoveAll(dir)
	}
	// A directory without the finished marker is a clone whose creation was
	// interrupted: remove it, or the rename below could never succeed.
	if _, err := os.Lstat(dir); err == nil {
		_ = os.RemoveAll(dir)
	}
	// The credential is chosen before the reservation, and a failure is
	// remembered per (scope, credential): one project's anonymous attempt on
	// a private repository must not block, for an hour, the clone another
	// project's credential would make (push security review of 180ed1da9).
	marker, header := a.credentialFor(ctx, projectID, t)
	failKey := key + "|" + marker
	if !a.reserve(failKey) {
		a.markUnmade(key)
		a.refuse(projectID, scope, "a clone limit, or a failed clone with this credential remembered for an hour", "see the doctor's clone count and the provenance root's size")
		return "", ProvReasonCloneFailed
	}
	err := a.create(ctx, dir, t, header)
	a.release()
	if err == nil {
		err = writeCredentialMarker(dir, marker)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		a.mu.Lock()
		a.failed[failKey] = a.now()
		a.mu.Unlock()
		a.markUnmade(key)
		a.refuse(projectID, scope, "the clone failed: "+firstErrLine(err), "check the forge's base_url and credential, and that the repository exists")
		return "", ProvReasonCloneFailed
	}
	touchLastUsed(dir)
	a.mu.Lock()
	a.fetched[key] = a.now()
	a.mu.Unlock()
	return dir, ""
}

// autoCloneEvictAfter is how long a clone must have gone unused before it may
// be evicted to make room at the count limit. Evicting is safe for trust: a
// record whose clone is missing is left unchanged (#506 F4), and the clone is
// re-created on its next use.
const autoCloneEvictAfter = 7 * 24 * time.Hour

const lastUsedMarker = "vornik-provenance-last-used"

func touchLastUsed(dir string) {
	p := filepath.Join(dir, ".git", lastUsedMarker)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		_ = os.WriteFile(p, nil, 0o600)
	}
}

func lastUsed(dir string) time.Time {
	if fi, err := os.Stat(filepath.Join(dir, ".git", lastUsedMarker)); err == nil {
		return fi.ModTime()
	}
	if fi, err := os.Stat(dir); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// evictLRULocked removes the least recently used clone when it has gone
// unused for autoCloneEvictAfter, so scopes nobody uses any more cannot hold
// every slot. It reports whether a slot was freed.
func (a *AutoClones) evictLRULocked() bool {
	var oldest string
	var at time.Time
	for _, c := range a.listLocked() {
		if t := lastUsed(c.Dir); oldest == "" || t.Before(at) {
			oldest, at = c.Dir, t
		}
	}
	if oldest == "" || a.now().Sub(at) < autoCloneEvictAfter {
		return false
	}
	return os.RemoveAll(oldest) == nil
}

func (a *AutoClones) lockFor(key string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.scopeMu[key]
	if !ok {
		m = &sync.Mutex{}
		a.scopeMu[key] = m
	}
	return m
}

// reserve applies the failure memory, keyed by (scope, credential marker),
// and the count and total-size limits.
func (a *AutoClones) reserve(failKey string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at, ok := a.failed[failKey]; ok && a.now().Sub(at) < orDefault(a.FailMemory, DefaultAutoCloneFailMemory) {
		return false
	}
	if len(a.listLocked())+a.pending >= orDefault(a.MaxClones, DefaultAutoCloneMax) && !a.evictLRULocked() {
		return false
	}
	// In-flight creations count at their maximum size, so concurrent
	// creations cannot pass the total together (code review 5420 F3).
	inFlight := int64(a.pending) * orDefault(a.MaxCloneBytes, DefaultAutoCloneMaxBytes)
	if dirBytes(a.Root)+inFlight >= orDefault(a.MaxTotalBytes, DefaultAutoCloneTotalBytes) {
		return false
	}
	a.pending++
	return true
}

func (a *AutoClones) release() {
	a.mu.Lock()
	a.pending--
	a.mu.Unlock()
}

// create clones into a temporary sibling and renames it into place, so a
// half-made clone is never visible as one.
func (a *AutoClones) create(ctx context.Context, dir string, t scopeTarget, header string) error {
	select {
	case a.createSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-a.createSem }()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".clone-")
	if err != nil {
		return err
	}
	_ = os.Remove(tmp) // git clone wants a destination that does not exist
	defer func() { _ = os.RemoveAll(tmp) }()
	// The size limit holds DURING the clone, not only after it (security
	// review 2026-10-23): a watcher cancels the clone as soon as the
	// temporary directory passes the limit, so a huge repository cannot fill
	// the disk first. The temporary directory lies under Root, so it also
	// counts against the total limit of concurrent reservations.
	limit := orDefault(a.MaxCloneBytes, DefaultAutoCloneMaxBytes)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var tooLarge atomic.Bool
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if dirBytes(tmp) > limit {
					tooLarge.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	err = a.Run.Clone(cctx, a.Root, t.cloneURL, tmp, header)
	close(done)
	if tooLarge.Load() || dirBytes(tmp) > limit {
		return errCloneTooLarge
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

type cloneError string

func (e cloneError) Error() string { return string(e) }

const errCloneTooLarge = cloneError("provenance clone exceeds the size limit")

// freshen fetches the clone's default branch when the last fetch is older
// than FetchAfter. A failed fetch leaves the clone as it was: the version is
// still examined against the state already fetched (#506 C4).
func (a *AutoClones) freshen(ctx context.Context, key, dir, header string) {
	a.mu.Lock()
	last := a.fetched[key]
	a.mu.Unlock()
	if a.now().Sub(last) < orDefault(a.FetchAfter, DefaultAutoCloneFetchAfter) {
		return
	}
	select {
	case a.fetchSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-a.fetchSem }()
	a.mu.Lock()
	a.fetched[key] = a.now()
	a.mu.Unlock()
	branch, err := a.Run.OriginHead(ctx, dir)
	if err != nil || !plainBranchName(branch) {
		return
	}
	_ = a.Run.Fetch(ctx, dir, branch, header)
}

// Freshen fetches a clone that is not one of the automatic ones (a
// configured memory.provenance_clone) when its last fetch is older than
// FetchAfter, under the same fetch slots. Its credentials are git's own
// composed helpers.
func (a *AutoClones) Freshen(ctx context.Context, dir string) {
	a.init()
	key := "dir:" + dir
	lock := a.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	a.freshen(ctx, key, dir, "")
}

// List returns the automatic clones on disk, for the sweep's fetch and the
// doctor.
func (a *AutoClones) List() []ProvenanceClone {
	a.init()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listLocked()
}

func (a *AutoClones) listLocked() []ProvenanceClone {
	var out []ProvenanceClone
	hosts, _ := os.ReadDir(a.Root)
	for _, h := range hosts {
		if !h.IsDir() || strings.HasPrefix(h.Name(), ".") {
			continue
		}
		owners, _ := os.ReadDir(filepath.Join(a.Root, h.Name()))
		for _, o := range owners {
			if !o.IsDir() || strings.HasPrefix(o.Name(), ".") {
				continue
			}
			// A forge clone (#508): <prefix joined by +>/<repo joined by +>.
			if two := filepath.Join(a.Root, h.Name(), o.Name()); isClone(two) {
				out = append(out, ProvenanceClone{Dir: two, RepoScope: strings.ReplaceAll(h.Name(), "+", "/") + "/" + strings.ReplaceAll(o.Name(), "+", "/")})
				continue
			}
			repos, _ := os.ReadDir(filepath.Join(a.Root, h.Name(), o.Name()))
			for _, r := range repos {
				dir := filepath.Join(a.Root, h.Name(), o.Name(), r.Name())
				if r.IsDir() && !strings.HasPrefix(r.Name(), ".") && isClone(dir) {
					out = append(out, ProvenanceClone{Dir: dir, RepoScope: h.Name() + "/" + o.Name() + "/" + r.Name()})
				}
			}
		}
	}
	return out
}

// dirBytes is the apparent size of the files under dir.
func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, ierr := d.Info(); ierr == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// SpawnCloneRunner runs the automatic clones' git through the process-spawn
// law's provenance kind (spawn.GitProvenanceClone, spawn.GitProvenance).
type SpawnCloneRunner struct {
	// Timeout bounds one clone or fetch (DefaultProvenanceFetchTimeout).
	Timeout time.Duration
}

func (r SpawnCloneRunner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultProvenanceFetchTimeout
}

// Clone runs the pinned blob-less clone.
func (r SpawnCloneRunner) Clone(ctx context.Context, root, url, dest, authHeader string) error {
	cctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd, err := spawn.GitProvenanceClone(cctx, root, url, dest)
	if err != nil {
		return err
	}
	return runProvenanceCmd(cmd, authHeader)
}

// Fetch fetches one branch into its tracking ref.
func (r SpawnCloneRunner) Fetch(ctx context.Context, dir, branch, authHeader string) error {
	fctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd, err := spawn.GitProvenance(fctx, dir, "fetch", "--quiet", "origin",
		"+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	if err != nil {
		return err
	}
	return runProvenanceCmd(cmd, authHeader)
}

// OriginHead reads the default branch the clone recorded from origin.
func (r SpawnCloneRunner) OriginHead(ctx context.Context, dir string) (string, error) {
	cmd, err := spawn.GitProvenance(ctx, dir, "rev-parse", "--symbolic-full-name", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	b, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "refs/remotes/origin/")
	if !ok || b == "HEAD" {
		return "", cloneError("origin has no default branch")
	}
	return b, nil
}

func runProvenanceCmd(cmd *spawn.Cmd, authHeader string) error {
	if authHeader != "" {
		if err := cmd.WithGitHTTPAuthHeader(authHeader); err != nil {
			return err
		}
	}
	var stderr strings.Builder
	cmd.SetStderr(&stderr)
	if err := cmd.Run(); err != nil {
		// git's stderr never carries the header: it travels in GIT_CONFIG_*.
		return cloneError("git: " + err.Error() + ": " + strings.TrimSpace(stderr.String()))
	}
	return nil
}

// AutoCloneRoot is where the automatic clones live: beside the workspace
// root (never under it, where agents' worktrees are). "" when there is no
// workspace root. The daemon and the operator backfill both use it.
func AutoCloneRoot(workspaceRoot string) string {
	if strings.TrimSpace(workspaceRoot) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(filepath.Clean(workspaceRoot)), "provenance")
}

// ScopedCloneFor is the one per-scope clone resolution (#506 C1), used by
// the daemon and the operator backfill. own reports a project's configured
// clone: its repository scope, the resolved directory, and whether the
// configured path was refused. A scope that matches the project's own
// repository (case-insensitively, so a case variant cannot escape it) uses
// the configured clone, and a refused one is unsafe_clone, never no_clone.
// Any other scope uses its automatic clone.
func ScopedCloneFor(own func(projectID string) (scope, dir string, refused bool), auto *AutoClones) func(context.Context, string, string) (string, string) {
	return func(ctx context.Context, projectID, scope string) (string, string) {
		if s, dir, refused := own(projectID); s != "" && strings.EqualFold(scope, s) {
			if refused || dir == "" {
				return "", ProvReasonUnsafeClone
			}
			// A configured clone is fetched before verifying too, so a
			// design ingested after its push verifies (live check
			// 2026-10-23).
			if auto != nil {
				auto.Freshen(ctx, dir)
			}
			return dir, ""
		}
		if auto == nil {
			return "", ProvReasonNoClone
		}
		return auto.Ensure(ctx, projectID, scope)
	}
}

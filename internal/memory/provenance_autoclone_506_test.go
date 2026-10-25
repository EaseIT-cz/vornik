package memory

// GitHub #506 (P1, release blocker), design 22 "Amendment 2026-10-22
// (#506)" C1/C2: every repository a project ingests from gets its own
// daemon-only clone, created on the first ingest and fetched before
// verifying, with no configuration. The runner here clones a local
// repository, so the manager's rules (path, host allowlist, limits, failure
// memory, freshness) are exercised without a network. Found at: production
// (operator observation 2026-10-22: no companion design verified).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// localCloneRunner clones from a local origin regardless of the URL, and
// counts what it was asked to do.
type localCloneRunner struct {
	origin                string
	clones                atomic.Int32
	fetches               atomic.Int32
	fail                  error
	block                 chan struct{}
	inFlight, maxInFlight atomic.Int32
}

func (r *localCloneRunner) Clone(ctx context.Context, _, _, dest, _ string) error {
	r.clones.Add(1)
	n := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for {
		m := r.maxInFlight.Load()
		if n <= m || r.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}
	if r.block != nil {
		<-r.block
	}
	if r.fail != nil {
		return r.fail
	}
	return exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-checkout", r.origin, dest).Run()
}

func (r *localCloneRunner) Fetch(ctx context.Context, dir, branch, _ string) error {
	r.fetches.Add(1)
	return exec.CommandContext(ctx, "git", "-C", dir, "fetch", "--quiet", "origin",
		"+refs/heads/"+branch+":refs/remotes/origin/"+branch).Run()
}

func (r *localCloneRunner) OriginHead(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"), nil
}

func newOrigin(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	for _, args := range [][]string{
		{"init", "--quiet", "-b", "main", dir},
		{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func newAutoClones(t *testing.T, r CloneRunner) *AutoClones {
	t.Helper()
	return &AutoClones{Root: filepath.Join(t.TempDir(), "provenance"), Hosts: []string{"github.com"}, Run: r}
}

func TestAutoClones506_CreatesOneCloneByScopeOnFirstUse(t *testing.T) {
	ctx := context.Background()
	r := &localCloneRunner{origin: newOrigin(t)}
	a := newAutoClones(t, r)

	dir, reason := a.Ensure(ctx, "p", "github.com/acme/widgets")
	if reason != "" || dir != filepath.Join(a.Root, "github.com", "acme", "widgets") {
		t.Fatalf("Ensure = %q, %q", dir, reason)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("no clone at %s: %v", dir, err)
	}
	// A second scope gets its own clone; the first is reused, not re-cloned.
	if _, reason := a.Ensure(ctx, "p", "github.com/acme/gadgets"); reason != "" {
		t.Fatal(reason)
	}
	if _, reason := a.Ensure(ctx, "p", "github.com/acme/widgets"); reason != "" {
		t.Fatal(reason)
	}
	if n := r.clones.Load(); n != 2 {
		t.Fatalf("clones = %d, want 2 (one per scope)", n)
	}
	if got := len(a.List()); got != 2 {
		t.Fatalf("List = %d clones, want 2", got)
	}
}

func TestAutoClones506_RefusesScopesThatAreNotARepositoryOnAListedHost(t *testing.T) {
	ctx := context.Background()
	r := &localCloneRunner{origin: newOrigin(t)}
	a := newAutoClones(t, r)
	// A well-formed scope on an unlisted host may be a self-hosted forge: no
	// clone is made, and it is clone_failed (confined to its own scope).
	for _, scope := range []string{"evil.example/acme/widgets", "github.com.evil.example/a/b"} {
		if dir, reason := a.Ensure(ctx, "p", scope); reason != ProvReasonCloneFailed || dir != "" {
			t.Errorf("%q: Ensure = %q, %q; want clone_failed", scope, dir, reason)
		}
	}
	// Anything that cannot name a repository fails closed (security review
	// 2026-10-23): scope_not_cloneable is an examined failure.
	if ProvenanceUnexaminable(ProvReasonScopeNotCloneable) {
		t.Fatal("a malformed scope counts as unexaminable")
	}
	for _, scope := range []string{
		"github.com/acme",               // not owner/repo
		"github.com/acme/widgets/extra", // too deep
		"github.com/../etc",             // traversal
		"github.com/acme/..",            // traversal
		"github.com/acme/wid gets",      // odd character
		"user@github.com/acme/widgets",  // userinfo
		"github.com:8443/acme/widgets",  // port
		"",                              // empty
		"*",                             // the cross-cutting scope
	} {
		if dir, reason := a.Ensure(ctx, "p", scope); reason != ProvReasonScopeNotCloneable || dir != "" {
			t.Errorf("%q: Ensure = %q, %q; want scope_not_cloneable", scope, dir, reason)
		}
	}
	if n := r.clones.Load(); n != 0 {
		t.Fatalf("a refused scope reached git %d times", n)
	}
}

func TestAutoClones506_FailedCreateIsRememberedForAnHour(t *testing.T) {
	ctx := context.Background()
	r := &localCloneRunner{origin: newOrigin(t), fail: errors.New("repository not found")}
	a := newAutoClones(t, r)
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, reason := a.Ensure(ctx, "p", "github.com/acme/private"); reason != ProvReasonCloneFailed {
			t.Fatalf("attempt %d: reason %q", i, reason)
		}
	}
	if n := r.clones.Load(); n != 1 {
		t.Fatalf("clone attempts within the hour = %d, want 1", n)
	}
	now = now.Add(61 * time.Minute)
	r.fail = nil
	if _, reason := a.Ensure(ctx, "p", "github.com/acme/private"); reason != "" {
		t.Fatalf("after the hour: %q", reason)
	}
}

func TestAutoClones506_Limits(t *testing.T) {
	ctx := context.Background()
	r := &localCloneRunner{origin: newOrigin(t)}
	a := newAutoClones(t, r)
	a.MaxClones = 2
	for _, s := range []string{"github.com/a/one", "github.com/a/two"} {
		if _, reason := a.Ensure(ctx, "p", s); reason != "" {
			t.Fatal(reason)
		}
	}
	if _, reason := a.Ensure(ctx, "p", "github.com/a/three"); reason != ProvReasonCloneFailed {
		t.Fatalf("a clone past the count limit: %q", reason)
	}

	// A clone over the size limit is removed and refused.
	b := newAutoClones(t, r)
	b.MaxCloneBytes = 1
	if dir, reason := b.Ensure(ctx, "p", "github.com/a/big"); reason != ProvReasonCloneFailed || dir != "" {
		t.Fatalf("an oversized clone: %q %q", dir, reason)
	}
	if _, err := os.Stat(filepath.Join(b.Root, "github.com", "a", "big")); !os.IsNotExist(err) {
		t.Fatalf("the oversized clone was left on disk: %v", err)
	}
}

func TestAutoClones506_AtMostTwoCreationsAtOnce(t *testing.T) {
	ctx := context.Background()
	r := &localCloneRunner{origin: newOrigin(t), block: make(chan struct{})}
	a := newAutoClones(t, r)
	var wg sync.WaitGroup
	for _, s := range []string{"github.com/c/one", "github.com/c/two", "github.com/c/three", "github.com/c/four"} {
		wg.Add(1)
		go func(s string) { defer wg.Done(); a.Ensure(ctx, "p", s) }(s)
	}
	time.Sleep(200 * time.Millisecond)
	close(r.block)
	wg.Wait()
	if m := r.maxInFlight.Load(); m > 2 {
		t.Fatalf("%d clones ran at once, want at most 2", m)
	}
}

func TestAutoClones506_FetchesBeforeVerifyingWhenStale(t *testing.T) {
	ctx := context.Background()
	r := &localCloneRunner{origin: newOrigin(t)}
	a := newAutoClones(t, r)
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	if _, reason := a.Ensure(ctx, "p", "github.com/acme/widgets"); reason != "" {
		t.Fatal(reason)
	}
	// Fresh from the clone: no fetch.
	a.Ensure(ctx, "p", "github.com/acme/widgets")
	if n := r.fetches.Load(); n != 0 {
		t.Fatalf("fetched %d times right after cloning", n)
	}
	now = now.Add(3 * time.Minute)
	a.Ensure(ctx, "p", "github.com/acme/widgets")
	a.Ensure(ctx, "p", "github.com/acme/widgets")
	if n := r.fetches.Load(); n != 1 {
		t.Fatalf("fetches after 3 minutes = %d, want exactly 1 (2-minute limit)", n)
	}
}

// The count check and the create are one reservation: concurrent first
// ingests of different scopes cannot overshoot the limit (review 28a5 F5).
func TestAutoClones506_CountLimitHoldsUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	r := &localCloneRunner{origin: newOrigin(t)}
	a := newAutoClones(t, r)
	a.MaxClones = 3
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); a.Ensure(ctx, "p", "github.com/race/r"+string(rune('a'+i))) }(i)
	}
	wg.Wait()
	if n := len(a.List()); n > 3 {
		t.Fatalf("%d clones exist, limit 3", n)
	}
}

// The size limit is enforced while the clone runs (security review
// 2026-10-23), and a clone unused for a week is evicted to make room.
func TestAutoClones506_SizeLimitDuringCloneAndEviction(t *testing.T) {
	ctx := context.Background()
	big := &bigCloneRunner{}
	a := newAutoClones(t, big)
	a.MaxCloneBytes = 1 << 20
	if _, reason := a.Ensure(ctx, "p", "github.com/a/huge"); reason != ProvReasonCloneFailed {
		t.Fatalf("a clone growing past the limit: %q", reason)
	}
	if !big.cancelled.Load() {
		t.Fatal("the clone was not cancelled while it ran")
	}

	r := &localCloneRunner{origin: newOrigin(t)}
	b := newAutoClones(t, r)
	b.MaxClones = 1
	if _, reason := b.Ensure(ctx, "p", "github.com/a/old"); reason != "" {
		t.Fatal(reason)
	}
	old := filepath.Join(b.Root, "github.com", "a", "old")
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(old, ".git", lastUsedMarker), stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, reason := b.Ensure(ctx, "p", "github.com/a/new"); reason != "" {
		t.Fatalf("no slot was freed from a clone unused for 8 days: %q", reason)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the unused clone was not evicted")
	}
}

// bigCloneRunner writes into the destination until its context is cancelled.
type bigCloneRunner struct{ cancelled atomic.Bool }

func (r *bigCloneRunner) Clone(ctx context.Context, _, _, dest, _ string) error {
	if err := os.MkdirAll(filepath.Join(dest, ".git"), 0o700); err != nil {
		return err
	}
	chunk := make([]byte, 256<<10)
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			r.cancelled.Store(true)
			return ctx.Err()
		default:
		}
		if i < 64 {
			_ = os.WriteFile(filepath.Join(dest, fmt.Sprintf("f%d", i)), chunk, 0o600)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func (r *bigCloneRunner) Fetch(context.Context, string, string, string) error { return nil }
func (r *bigCloneRunner) OriginHead(context.Context, string) (string, error)  { return "main", nil }

// Live check after deploy (2026-10-23): a design ingested after its push into
// a project with a CONFIGURED clone stayed unverified, because only automatic
// clones were fetched before verifying. ScopedCloneFor now freshens the
// configured clone too, with the same 2-minute limit.
func TestScopedCloneFor506_FreshensAConfiguredClone(t *testing.T) {
	ctx := context.Background()
	r := &localCloneRunner{origin: newOrigin(t)}
	a := newAutoClones(t, r)
	configured := filepath.Join(a.Root, "configured")
	if out, err := exec.Command("git", "clone", "--quiet", r.origin, configured).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	now := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	resolve := ScopedCloneFor(func(string) (string, string, bool) { return "github.com/acme/own", configured, false }, a)
	if dir, reason := resolve(ctx, "p", "github.com/acme/own"); dir != configured || reason != "" {
		t.Fatalf("resolve = %q %q", dir, reason)
	}
	resolve(ctx, "p", "github.com/acme/own")
	if n := r.fetches.Load(); n != 1 {
		t.Fatalf("fetches = %d, want 1 (first use fetches, the second is within 2 minutes)", n)
	}
	now = now.Add(3 * time.Minute)
	resolve(ctx, "p", "github.com/acme/own")
	if n := r.fetches.Load(); n != 2 {
		t.Fatalf("fetches after 3 minutes = %d, want 2", n)
	}
}

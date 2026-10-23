package memory

// GitHub #499 (operator decision A18; design 22, "Amendment 2026-10-21
// (#482)", part 8g): the hourly sweep fetches the trusted branch into each
// daemon-only provenance clone, bounded by a timeout, with every outcome
// counted; and a configured clone that resolves into the agent-mounted
// workspace root is refused.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memoryscope"
)

// daemonCloneFixture is an upstream and a daemon-only clone of it, outside
// any workspace root.
func daemonCloneFixture(t *testing.T) (upstream, clone, scope string, v *ProvenanceVerifier) {
	t.Helper()
	base := t.TempDir()
	upstream = filepath.Join(base, "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	provGit(t, upstream, "init", "-q", "-b", "main")
	provWrite(t, upstream, provLLD, provLLDBody)
	provGit(t, upstream, "add", "-A")
	provGit(t, upstream, "commit", "-q", "-m", "init")
	daemon := filepath.Join(base, "daemon")
	if err := os.MkdirAll(daemon, 0o755); err != nil {
		t.Fatal(err)
	}
	provGit(t, daemon, "clone", "-q", upstream, "proj")
	clone = filepath.Join(daemon, "proj")
	v = NewProvenanceVerifier(filepath.Join(base, "workspaces"))
	v.CloneDir = func(string) string { return clone }
	return upstream, clone, memoryscope.NormalizeRemote(upstream), v
}

func newFetchSweeper(v *ProvenanceVerifier, clone, scope string) (*ProvenanceSweeper, *Metrics) {
	m := NewMetrics(prometheus.NewRegistry())
	return &ProvenanceSweeper{
		Verifier: v,
		Logger:   zerolog.Nop(),
		Metrics:  m,
		Clones:   func() []ProvenanceClone { return []ProvenanceClone{{ProjectID: "proj", Dir: clone, RepoScope: scope}} },
	}, m
}

// A design merged on origin after the clone was made verifies only once the
// sweep has fetched it: nobody else touches a daemon-only clone.
func TestProvenanceSweep_FetchesDaemonClone(t *testing.T) {
	upstream, clone, scope, v := daemonCloneFixture(t)
	const body = "# Design 22\n\nThe rule, amended.\n"
	provWrite(t, upstream, provLLD, body)
	provGit(t, upstream, "add", "-A")
	provGit(t, upstream, "commit", "-q", "-m", "amend")
	req := ProvenanceRequest{ProjectID: "proj", RepoScope: scope, Path: provLLD, Content: []byte(body)}
	if r := v.Verify(context.Background(), req); r.Verified() {
		t.Fatalf("verified before any fetch: %+v", r)
	}
	s, m := newFetchSweeper(v, clone, scope)
	s.FetchClones(context.Background())
	if r := v.Verify(context.Background(), req); !r.Verified() {
		t.Fatalf("not verified after the sweep's fetch: %+v", r)
	}
	if got := testutil.ToFloat64(m.ProvenanceFetchTotal.WithLabelValues("proj", ProvenanceFetchOK)); got != 1 {
		t.Fatalf("ok fetches counted %v, want 1", got)
	}
	last := s.LastFetches()
	if len(last) != 1 || last[0].Outcome != ProvenanceFetchOK || last[0].Branch != "main" || last[0].At.IsZero() {
		t.Fatalf("last fetch: %+v", last)
	}
}

// A failed fetch, a fetch that outlives its timeout, and an unresolvable
// trusted branch are each counted under their own outcome, and the doctor
// sees the last one.
func TestProvenanceSweep_FetchFailuresAreCounted(t *testing.T) {
	upstream, clone, scope, v := daemonCloneFixture(t)

	if err := os.RemoveAll(upstream); err != nil {
		t.Fatal(err)
	}
	s, m := newFetchSweeper(v, clone, scope)
	s.FetchClones(context.Background())
	if got := testutil.ToFloat64(m.ProvenanceFetchTotal.WithLabelValues("proj", ProvenanceFetchFailed)); got != 1 {
		t.Fatalf("failed fetches counted %v, want 1 (%+v)", got, s.LastFetches())
	}
	if l := s.LastFetches(); len(l) != 1 || l[0].Error == "" {
		t.Fatalf("failure not recorded with its error: %+v", l)
	}

	spawnGit := v.Git
	v.Git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "fetch" {
			<-ctx.Done()
			return nil, errors.New("git fetch: killed")
		}
		return spawnGit(ctx, dir, args...)
	}
	s.FetchTimeout = 20 * time.Millisecond
	s.FetchClones(context.Background())
	if got := testutil.ToFloat64(m.ProvenanceFetchTotal.WithLabelValues("proj", ProvenanceFetchTimeout)); got != 1 {
		t.Fatalf("timed-out fetches counted %v, want 1 (%+v)", got, s.LastFetches())
	}

	v.DefaultBranch = func(string) string { return "trunk" } // disagrees with origin/HEAD
	s.FetchClones(context.Background())
	if got := testutil.ToFloat64(m.ProvenanceFetchTotal.WithLabelValues("proj", ProvenanceFetchNoBranch)); got != 1 {
		t.Fatalf("no_branch counted %v, want 1", got)
	}

	v.CloneDir = func(string) string { return "" }
	s.FetchClones(context.Background())
	if got := testutil.ToFloat64(m.ProvenanceFetchTotal.WithLabelValues("proj", ProvenanceFetchNoClone)); got != 1 {
		t.Fatalf("no_clone counted %v, want 1", got)
	}
}

// A daemon-only clone that is, or resolves into, the agent-mounted
// workspace root is refused; so is a root inside the clone.
func TestDaemonCloneDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspaces")
	inside := filepath.Join(root, "proj")
	outside := filepath.Join(base, "daemon", "proj")
	for _, d := range []string{inside, filepath.Join(outside, ".git", "objects"), filepath.Join(outside, ".git", "refs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, ".git", "config"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "looks-outside")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(base, "root-link")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(outside)
	for _, tc := range []struct {
		name, clone, root, want string
	}{
		{"outside the root", outside, root, want},
		{"inside the root", inside, root, ""},
		{"the root itself", root, root, ""},
		{"a link into the root", link, root, ""},
		{"inside the root named through a link", inside, rootLink, ""},
		{"a clone above the root", base, root, ""},
		{"relative", "daemon/proj", root, ""},
		{"missing", filepath.Join(base, "nope"), root, ""},
		{"no workspace root", outside, "", want},
	} {
		if got := DaemonCloneDir(tc.clone, tc.root); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Review 05cc F2: the origin is checked before every fetch, not only when
// the config loaded, so a clone whose origin was repointed is never fetched
// from; F1: a clone writable by group or other is refused.
func TestProvenanceSweep_FetchChecksTheCloneFirst(t *testing.T) {
	_, clone, scope, v := daemonCloneFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	provGit(t, elsewhere, "init", "-q", "-b", "main")
	provGit(t, clone, "remote", "set-url", "origin", elsewhere)
	fetched := false
	spawnGit := v.Git
	v.Git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "fetch" {
			fetched = true
		}
		return spawnGit(ctx, dir, args...)
	}
	s, m := newFetchSweeper(v, clone, scope)
	s.FetchClones(context.Background())
	if fetched {
		t.Fatal("fetched from a repointed origin")
	}
	if got := testutil.ToFloat64(m.ProvenanceFetchTotal.WithLabelValues("proj", ProvenanceFetchRefused)); got != 1 {
		t.Fatalf("refused fetches counted %v, want 1 (%+v)", got, s.LastFetches())
	}
	if l := s.LastFetches(); len(l) != 1 || l[0].Error != ProvReasonRepoMismatch {
		t.Fatalf("refusal not recorded with its reason: %+v", l)
	}
}

func TestDaemonCloneDir_RefusesSharedWritableClone(t *testing.T) {
	_, clone, _, _ := daemonCloneFixture(t)
	if DaemonCloneDir(clone, "") == "" {
		t.Fatal("a daemon-only clone was refused")
	}
	for _, rel := range []string{"", ".git", ".git/objects", ".git/refs"} {
		p := filepath.Join(clone, rel)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, fi.Mode().Perm()|0o002); err != nil {
			t.Fatal(err)
		}
		if got := DaemonCloneDir(clone, ""); got != "" {
			t.Errorf("%q world-writable: accepted %q", rel, got)
		}
		if err := os.Chmod(p, fi.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(clone, ".git", "config")
	if err := os.Chmod(cfg, 0o664); err != nil {
		t.Fatal(err)
	}
	if got := DaemonCloneDir(clone, ""); got != "" {
		t.Errorf("group-writable .git/config: accepted %q", got)
	}
}

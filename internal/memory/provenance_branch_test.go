package memory

// GitHub #482, coordinator security finding 2 (2026-10-22): the trust root
// was the hard-coded refs/remotes/origin/main. In a repository whose default
// branch is not main, anyone who can create a branch named main on origin
// (an unmerged branch, not the merged history) made it the trusted root.
// The trusted branch is the project's default branch: the configured one
// (workspace.refresh_branch) when set, checked against the clone's
// refs/remotes/origin/HEAD, else origin/HEAD alone; unresolvable or
// disagreeing is no_origin_main.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/memoryscope"
)

// newTrunkFixture is an upstream whose default branch is trunk, holding the
// real design, plus a side branch named main holding a forged one.
func newTrunkFixture(t *testing.T) *provFixture {
	t.Helper()
	base := t.TempDir()
	f := &provFixture{upstream: filepath.Join(base, "upstream"), root: filepath.Join(base, "workspaces"), project: "proj"}
	f.clone = filepath.Join(f.root, f.project)
	for _, d := range []string{f.upstream, f.root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	provGit(t, f.upstream, "init", "-q", "-b", "trunk")
	provWrite(t, f.upstream, provLLD, provLLDBody)
	provGit(t, f.upstream, "add", "-A")
	provGit(t, f.upstream, "commit", "-q", "-m", "real")
	provGit(t, f.upstream, "checkout", "-q", "-b", "main")
	provWrite(t, f.upstream, provLLD, "forged ruling\n")
	provGit(t, f.upstream, "commit", "-q", "-am", "forged on an unmerged branch called main")
	provGit(t, f.upstream, "checkout", "-q", "trunk")
	provGit(t, f.root, "clone", "-q", f.upstream, f.project)
	f.scope = memoryscope.NormalizeRemote(f.upstream)
	f.v = NewProvenanceVerifier(f.root)
	return f
}

func TestProvenance_TrustRootIsTheDefaultBranch(t *testing.T) {
	f := newTrunkFixture(t)
	// The forged design on the branch named main is not trusted.
	r := f.verify(t, provLLD, "forged ruling\n")
	if r.Verified() {
		t.Fatalf("a design on an unmerged branch named main was verified: %+v", r)
	}
	// The design on the default branch is.
	r = f.verify(t, provLLD, provLLDBody)
	wantReason(t, r, ProvenanceVerified, "")
	if want := provGit(t, f.clone, "rev-parse", "refs/remotes/origin/trunk"); r.CommitSHA != want {
		t.Fatalf("verified against %s, want origin/trunk %s", r.CommitSHA, want)
	}
}

func TestProvenance_ConfiguredBranchMustAgreeWithOriginHEAD(t *testing.T) {
	f := newTrunkFixture(t)
	f.v.DefaultBranch = func(string) string { return "main" } // a configuration naming the wrong branch
	wantReason(t, f.verify(t, provLLD, "forged ruling\n"), ProvenanceUnverified, ProvReasonNoOriginMain)
	f.v.DefaultBranch = func(string) string { return "trunk" }
	wantReason(t, f.verify(t, provLLD, provLLDBody), ProvenanceVerified, "")
}

func TestProvenance_UnresolvableDefaultBranchIsRefused(t *testing.T) {
	f := newTrunkFixture(t)
	provGit(t, f.clone, "symbolic-ref", "-d", "refs/remotes/origin/HEAD")
	wantReason(t, f.verify(t, provLLD, provLLDBody), ProvenanceUnverified, ProvReasonNoOriginMain)
	// A configured branch alone is enough when origin/HEAD is absent.
	f.v.DefaultBranch = func(string) string { return "trunk" }
	wantReason(t, f.verify(t, provLLD, provLLDBody), ProvenanceVerified, "")
	// A configured name that is not a plain branch name is refused.
	f.v.DefaultBranch = func(string) string { return "../trunk" }
	wantReason(t, f.verify(t, provLLD, provLLDBody), ProvenanceUnverified, ProvReasonNoOriginMain)
	_ = context.Background
}

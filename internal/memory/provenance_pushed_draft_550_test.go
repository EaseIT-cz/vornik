package memory

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/testenv"
)

// GitHub #550 D3 (LLD 22, amendment 2026-11-08): 22-companion-rag-recall-remember.md
// was ingested 2026-10-24 as blob 06800c64a, which was pushed in 180ed1da9
// and replaced by e59b4c67e before a sweep saw it. The record stayed
// "a draft awaiting its push" for good, though the version had been pushed.

func TestProvenance_PushedThenReplacedDraftIsStale_550(t *testing.T) {
	f := newProvFixture(t)
	draft := "# Design 22\n\nThe draft rule.\n"
	f.upstreamCommit(t, provLLD, draft)
	pushed := provGit(t, f.clone, "rev-parse", "refs/remotes/origin/main")
	f.upstreamCommit(t, provLLD, "# Design 22\n\nThe rule, revised.\n")

	r := f.verify(t, provLLD, draft)
	wantReason(t, r, ProvenanceStale, ProvReasonBlobChanged)
	if r.CommitSHA != pushed {
		t.Fatalf("the stale record names the commit that pushed it: got %s, want %s", r.CommitSHA, pushed)
	}

	// A revert at the head verifies it on the next sweep.
	f.upstreamCommit(t, provLLD, draft)
	wantReason(t, f.v.Recheck(context.Background(), priorOf(f, r)), ProvenanceVerified, "")
}

func TestProvenance_NeverPushedDraftStaysAwaitingPush_550(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, provLLD, "# Design 22\n\nThe rule, revised.\n")
	wantReason(t, f.verify(t, provLLD, "# Design 22\n\nNever pushed.\n"), ProvenanceUnverified, ProvReasonBlobMismatch)
}

func TestProvenance_SameBytesAtAnotherPathDoNotCount_550(t *testing.T) {
	f := newProvFixture(t)
	body := "# Elsewhere\n"
	f.upstreamCommit(t, ProvenanceLLDDir+"/other.md", body)
	f.upstreamCommit(t, ProvenanceLLDDir+"/other.md", "# Elsewhere, revised\n")
	wantReason(t, f.verify(t, provLLD, body), ProvenanceUnverified, ProvReasonBlobMismatch)
}

func TestProvenance_MergeResolutionBlobIsFound_550(t *testing.T) {
	f := newProvFixture(t)
	resolved := "# Design 22\n\nResolved in the merge.\n"
	provGit(t, f.upstream, "checkout", "-q", "-b", "side")
	provWrite(t, f.upstream, provLLD, "# Design 22\n\nSide.\n")
	provGit(t, f.upstream, "commit", "-q", "-am", "side")
	provGit(t, f.upstream, "checkout", "-q", "main")
	provWrite(t, f.upstream, provLLD, "# Design 22\n\nMain.\n")
	provGit(t, f.upstream, "commit", "-q", "-am", "main")
	_, _ = gitAllowFail(f.upstream, "merge", "-q", "side")
	provWrite(t, f.upstream, provLLD, resolved)
	provGit(t, f.upstream, "commit", "-q", "-am", "merge, resolved")
	merge := provGit(t, f.upstream, "rev-parse", "HEAD")
	f.upstreamCommit(t, provLLD, "# Design 22\n\nAfter the merge.\n")

	r := f.verify(t, provLLD, resolved)
	wantReason(t, r, ProvenanceStale, ProvReasonBlobChanged)
	if r.CommitSHA != merge {
		t.Fatalf("a blob first produced by a merge resolution is found at the merge: got %s, want %s", r.CommitSHA, merge)
	}
}

// gitAllowFail runs git where a non-zero exit is expected (a conflicting merge).
func gitAllowFail(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = testenv.Clean("GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1")
	return cmd.CombinedOutput()
}

// A failing history lookup keeps the answer from before #550 D3.
func TestProvenance_HistoryLookupFailureKeepsBlobMismatch_550(t *testing.T) {
	f := newProvFixture(t)
	draft := "# Design 22\n\nThe draft rule.\n"
	f.upstreamCommit(t, provLLD, draft)
	f.upstreamCommit(t, provLLD, "# Design 22\n\nThe rule, revised.\n")
	gitRun := f.v.Git
	f.v.Git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-list" {
			return nil, errors.New("rev-list failed")
		}
		return gitRun(ctx, dir, args...)
	}
	wantReason(t, f.verify(t, provLLD, draft), ProvenanceUnverified, ProvReasonBlobMismatch)

	// rev-list answers, but resolving commit:path fails (review 6a0c S3).
	f.v.Git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" && strings.Contains(args[len(args)-1], ":") {
			return nil, errors.New("rev-parse failed")
		}
		return gitRun(ctx, dir, args...)
	}
	wantReason(t, f.verify(t, provLLD, draft), ProvenanceUnverified, ProvReasonBlobMismatch)
}

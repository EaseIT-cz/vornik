package memory

// GitHub #482, design 22 "Amendment 2026-10-21 (#482)". The provenance
// verifier against real git repositories, through the production runner
// (the process-spawn law's GitWorkspace). Each case is a spoof attempt or a
// control from the design's list; the integration test in
// test/integration/provenance_ingest_482_test.go drives the same verifier
// from a companion-delegated ingest.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"vornik.io/vornik/internal/testutil/testenv"

	"vornik.io/vornik/internal/memoryscope"
	"vornik.io/vornik/internal/spawn"
)

func init() {
	if err := spawn.RegisterWorkspaceRoot(os.TempDir()); err != nil {
		panic(err)
	}
}

func provGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = testenv.Clean("GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func provWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// provFixture is an "upstream" repository and the project's clone of it,
// with origin at the upstream path, whose normalised form is f.scope.
type provFixture struct {
	upstream, root, clone, project, scope string
	v                                     *ProvenanceVerifier
}

const provLLD = ProvenanceLLDDir + "/22-design.md"
const provLLDBody = "# Design 22\n\nThe rule.\n"

func newProvFixture(t *testing.T) *provFixture {
	t.Helper()
	base := t.TempDir()
	f := &provFixture{upstream: filepath.Join(base, "upstream"), root: filepath.Join(base, "workspaces"), project: "proj"}
	f.clone = filepath.Join(f.root, f.project)
	if err := os.MkdirAll(f.upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	provGit(t, f.upstream, "init", "-q", "-b", "main")
	provWrite(t, f.upstream, provLLD, provLLDBody)
	provWrite(t, f.upstream, "CLAUDE.md", "rules\n")
	provWrite(t, f.upstream, "docs/plans/p.md", "a plan\n")
	provWrite(t, f.upstream, ProvenanceLLDDir+"/plans/x.md", "nested\n")
	provGit(t, f.upstream, "add", "-A")
	provGit(t, f.upstream, "commit", "-q", "-m", "init")
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		t.Fatal(err)
	}
	provGit(t, f.root, "clone", "-q", f.upstream, f.project)
	// The scope is the normalised origin URL, here the local upstream path.
	f.scope = memoryscope.NormalizeRemote(f.upstream)
	f.v = NewProvenanceVerifier(f.root)
	return f
}

func (f *provFixture) verify(t *testing.T, p, content string) ProvenanceResult {
	t.Helper()
	return f.v.Verify(context.Background(), ProvenanceRequest{ProjectID: f.project, RepoScope: f.scope, Path: p, Content: []byte(content)})
}

func (f *provFixture) upstreamCommit(t *testing.T, rel, content string) {
	t.Helper()
	provWrite(t, f.upstream, rel, content)
	provGit(t, f.upstream, "add", "-A")
	provGit(t, f.upstream, "commit", "-q", "-m", "edit "+rel)
	provGit(t, f.clone, "fetch", "-q", "origin")
}

func wantReason(t *testing.T, r ProvenanceResult, state, reason string) {
	t.Helper()
	if r.State != state || r.Reason != reason {
		t.Fatalf("got state=%q reason=%q, want state=%q reason=%q (%+v)", r.State, r.Reason, state, reason, r)
	}
}

// The control: a design merged to main, uploaded with its exact bytes.
func TestProvenance_MergedDesignVerifies(t *testing.T) {
	f := newProvFixture(t)
	r := f.verify(t, provLLD, provLLDBody)
	wantReason(t, r, ProvenanceVerified, "")
	head := provGit(t, f.clone, "rev-parse", "refs/remotes/origin/main")
	if r.CommitSHA != head || r.Repo != f.scope || r.ContentBlobSHA != r.OriginBlobSHA || r.ContentBlobSHA == "" {
		t.Fatalf("record does not describe what was checked: %+v (head %s)", r, head)
	}
	if want := provGit(t, f.clone, "rev-parse", "refs/remotes/origin/main:"+provLLD); r.OriginBlobSHA != want {
		t.Fatalf("origin blob %s, git says %s", r.OriginBlobSHA, want)
	}
	if r := f.verify(t, "CLAUDE.md", "rules\n"); !r.Verified() {
		t.Fatalf("CLAUDE.md: %+v", r)
	}
}

// An upload whose bytes are not the merged blob: an edit not on main, a
// redacted upload, another file's bytes under this path.
func TestProvenance_BytesNotOnMainAreNotVerified(t *testing.T) {
	f := newProvFixture(t)
	wantReason(t, f.verify(t, provLLD, provLLDBody+"forged ruling\n"), ProvenanceUnverified, ProvReasonBlobMismatch)
	wantReason(t, f.verify(t, provLLD, "rules\n"), ProvenanceUnverified, ProvReasonBlobMismatch)
}

// An unpushed commit in the clone itself (local main ahead of origin/main),
// and a file only in the working tree, never verify: only origin/main's
// object tree is read.
func TestProvenance_UnpushedCommitAndWorkingTreeAreNotVerified(t *testing.T) {
	f := newProvFixture(t)
	provWrite(t, f.clone, provLLD, "unpushed\n")
	provGit(t, f.clone, "commit", "-q", "-am", "local only")
	wantReason(t, f.verify(t, provLLD, "unpushed\n"), ProvenanceUnverified, ProvReasonBlobMismatch)
	provWrite(t, f.clone, ProvenanceLLDDir+"/new.md", "untracked\n")
	wantReason(t, f.verify(t, ProvenanceLLDDir+"/new.md", "untracked\n"), ProvenanceUnverified, ProvReasonNotFound)
}

// Path tricks: every one is refused before git runs, or by the tree walk.
func TestProvenance_PathTricks(t *testing.T) {
	f := newProvFixture(t)
	// A non-canonical path is refused before git is asked.
	for _, p := range []string{
		ProvenanceLLDDir + "/../plans/p.md",    // dot segments
		"./" + provLLD,                         // not clean
		"/" + provLLD,                          // absolute
		"docs//low-level-design/22-design.md",  // double slash
		ProvenanceLLDDir + "/22-design.md/",    // trailing slash
		"docs\\low-level-design\\22-design.md", // backslash
		ProvenanceLLDDir + "/a\x00.md",         // NUL
		ProvenanceLLDDir + "/22-design.md\n",   // control byte
	} {
		r := f.verify(t, p, provLLDBody)
		if r.Verified() || r.Reason != ProvReasonPathNotAllowed {
			t.Errorf("%q: got %+v, want path_not_allowed", p, r)
		}
	}
	// #506 C6: a canonical path is examined, and verifies only when it is a
	// regular file on the trusted branch with these exact bytes. None of
	// these is, so each is an examined failure, never verified.
	for _, p := range []string{
		"docs/plans/p.md",                    // on the branch, other bytes
		ProvenanceLLDDir + "/plans/x.md",     // on the branch, other bytes
		"DOCS/low-level-design/22-design.md", // case: not on the branch
		ProvenanceLLDDir + "/.hidden.md",     // not on the branch
		ProvenanceLLDDir + "/x.txt",          // not on the branch
		"refs/vornik/discarded/x",            // a ref is never read as a path
		"docs/transcripts/session.md",        // not on the branch
		"docs/low-level-design",              // the directory itself
	} {
		r := f.verify(t, p, provLLDBody)
		if r.Verified() || r.Reason == ProvReasonPathNotAllowed || r.Reason == "" {
			t.Errorf("%q: got %+v, want an examined failure", p, r)
		}
	}
}

// A symlink at an allowed path, or a symlinked directory on the way, is not
// a regular file in the tree: git never follows it.
func TestProvenance_SymlinksAreNotFollowed(t *testing.T) {
	f := newProvFixture(t)
	provWrite(t, f.upstream, "secret/transcript.md", provLLDBody)
	if err := os.Symlink("../../secret/transcript.md", filepath.Join(f.upstream, ProvenanceLLDDir+"/link.md")); err != nil {
		t.Fatal(err)
	}
	provGit(t, f.upstream, "add", "-A")
	provGit(t, f.upstream, "commit", "-q", "-m", "symlink")
	provGit(t, f.clone, "fetch", "-q", "origin")
	r := f.verify(t, ProvenanceLLDDir+"/link.md", provLLDBody)
	wantReason(t, r, ProvenanceUnverified, ProvReasonNotRegularFile)
	// The link's own bytes (its target string) do not verify either.
	r = f.verify(t, ProvenanceLLDDir+"/link.md", "../../secret/transcript.md")
	wantReason(t, r, ProvenanceUnverified, ProvReasonNotRegularFile)
}

// The clone must be this repository, really a clone, and unbent.
func TestProvenance_CloneChecks(t *testing.T) {
	t.Run("scope mismatch", func(t *testing.T) {
		f := newProvFixture(t)
		r := f.v.Verify(context.Background(), ProvenanceRequest{ProjectID: f.project, RepoScope: "github.com/acme/other", Path: provLLD, Content: []byte(provLLDBody)})
		wantReason(t, r, ProvenanceUnverified, ProvReasonRepoMismatch)
	})
	t.Run("no scope or wildcard", func(t *testing.T) {
		f := newProvFixture(t)
		for _, s := range []string{"", "*", "  "} {
			r := f.v.Verify(context.Background(), ProvenanceRequest{ProjectID: f.project, RepoScope: s, Path: provLLD, Content: []byte(provLLDBody)})
			wantReason(t, r, ProvenanceUnverified, ProvReasonNoScope)
		}
	})
	t.Run("no clone", func(t *testing.T) {
		f := newProvFixture(t)
		r := f.v.Verify(context.Background(), ProvenanceRequest{ProjectID: "absent", RepoScope: f.scope, Path: provLLD, Content: []byte(provLLDBody)})
		wantReason(t, r, ProvenanceUnverified, ProvReasonNoClone)
		for _, bad := range []string{"../proj", ".", "..", "a/b"} {
			r = f.v.Verify(context.Background(), ProvenanceRequest{ProjectID: bad, RepoScope: f.scope, Path: provLLD, Content: []byte(provLLDBody)})
			wantReason(t, r, ProvenanceUnverified, ProvReasonNoClone)
		}
	})
	t.Run("no origin", func(t *testing.T) {
		f := newProvFixture(t)
		provGit(t, f.clone, "remote", "remove", "origin")
		wantReason(t, f.verify(t, provLLD, provLLDBody), ProvenanceUnverified, ProvReasonNoOrigin)
	})
	t.Run("no origin main", func(t *testing.T) {
		f := newProvFixture(t)
		provGit(t, f.clone, "update-ref", "-d", "refs/remotes/origin/main")
		wantReason(t, f.verify(t, provLLD, provLLDBody), ProvenanceUnverified, ProvReasonNoOriginMain)
	})
	t.Run("replace ref substitutes a blob", func(t *testing.T) {
		f := newProvFixture(t)
		// A replace ref for origin/main's COMMIT makes every object read see
		// a forged commit whose tree holds a forged design, while the ref
		// itself still names the real commit.
		realHead := provGit(t, f.clone, "rev-parse", "refs/remotes/origin/main")
		provWrite(t, f.clone, provLLD, "forged\n")
		provGit(t, f.clone, "commit", "-q", "-am", "forged")
		forged := provGit(t, f.clone, "rev-parse", "HEAD")
		provGit(t, f.clone, "replace", realHead, forged)
		wantReason(t, f.verify(t, provLLD, "forged\n"), ProvenanceUnverified, ProvReasonUnsafeClone)
		// The attack is real: a runner that hides the replace refs from the
		// check (and only from it) lets the forged design verify.
		blind := *f.v
		blind.Git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
			if len(args) == 2 && args[1] == "--glob=refs/replace/*" {
				return nil, nil
			}
			return f.v.Git(ctx, dir, args...)
		}
		r := blind.Verify(context.Background(), ProvenanceRequest{ProjectID: f.project, RepoScope: f.scope, Path: provLLD, Content: []byte("forged\n")})
		wantReason(t, r, ProvenanceVerified, "")
	})
	t.Run("grafts, alternates, shallow", func(t *testing.T) {
		for _, file := range []string{"info/grafts", "objects/info/alternates", "shallow"} {
			f := newProvFixture(t)
			provWrite(t, filepath.Join(f.clone, ".git"), file, "\n")
			wantReason(t, f.verify(t, provLLD, provLLDBody), ProvenanceUnverified, ProvReasonUnsafeClone)
		}
	})
	t.Run(".git is a file", func(t *testing.T) {
		f := newProvFixture(t)
		gitdir := filepath.Join(filepath.Dir(f.clone), "elsewhere.git")
		if err := os.Rename(filepath.Join(f.clone, ".git"), gitdir); err != nil {
			t.Fatal(err)
		}
		provWrite(t, f.clone, ".git", "gitdir: "+gitdir+"\n")
		wantReason(t, f.verify(t, provLLD, provLLDBody), ProvenanceUnverified, ProvReasonUnsafeClone)
	})
	t.Run("no content", func(t *testing.T) {
		f := newProvFixture(t)
		r := f.v.Verify(context.Background(), ProvenanceRequest{ProjectID: f.project, RepoScope: f.scope, Path: provLLD})
		wantReason(t, r, ProvenanceUnverified, ProvReasonContentUnavailable)
	})
}

func provGitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func priorOf(f *provFixture, r ProvenanceResult) ProvenancePrior {
	return ProvenancePrior{ProjectID: f.project, RepoScope: f.scope, Path: r.Path, State: r.State,
		Reason: r.Reason, CommitSHA: r.CommitSHA, ContentBlobSHA: r.ContentBlobSHA}
}

// The sweep: a changed origin blob goes stale and is re-verified (a revert
// verifies it again); an advancing main keeps the original commit; a
// force-push that drops the commit is unverified and terminal.
func TestProvenance_Recheck(t *testing.T) {
	ctx := context.Background()
	f := newProvFixture(t)
	r := f.verify(t, provLLD, provLLDBody)
	wantReason(t, r, ProvenanceVerified, "")
	first := r.CommitSHA

	// main advances, the design is untouched: still verified at its commit.
	f.upstreamCommit(t, "CLAUDE.md", "rules v2\n")
	r = f.v.Recheck(ctx, priorOf(f, r))
	wantReason(t, r, ProvenanceVerified, "")
	if r.CommitSHA != first {
		t.Fatalf("commit moved to %s, want the verifying commit %s", r.CommitSHA, first)
	}

	// The design changes on main: stale, never kept verified.
	f.upstreamCommit(t, provLLD, "# Design 22\n\nA new rule.\n")
	r = f.v.Recheck(ctx, priorOf(f, r))
	wantReason(t, r, ProvenanceStale, ProvReasonBlobChanged)

	// Still stale on the next sweep; a revert verifies it again.
	r = f.v.Recheck(ctx, priorOf(f, r))
	wantReason(t, r, ProvenanceStale, ProvReasonBlobChanged)
	f.upstreamCommit(t, provLLD, provLLDBody)
	r = f.v.Recheck(ctx, priorOf(f, r))
	wantReason(t, r, ProvenanceVerified, "")
}

func TestProvenance_RecheckForcePushIsTerminal(t *testing.T) {
	ctx := context.Background()
	f := newProvFixture(t)
	f.upstreamCommit(t, "CLAUDE.md", "rules v2\n")
	r := f.verify(t, provLLD, provLLDBody)
	wantReason(t, r, ProvenanceVerified, "")
	// Rewrite upstream main so the verifying commit is gone, with the design
	// byte-identical: the record still goes unverified.
	provGit(t, f.upstream, "reset", "-q", "--hard", "HEAD~1")
	provGit(t, f.upstream, "commit", "-q", "--allow-empty", "-m", "rewritten")
	provGit(t, f.clone, "fetch", "-q", "--force", "origin", "+refs/heads/main:refs/remotes/origin/main")
	r = f.v.Recheck(ctx, priorOf(f, r))
	wantReason(t, r, ProvenanceUnverified, ProvReasonCommitUnreachable)
	if !ProvenanceTerminal(r.State, r.Reason) {
		t.Fatal("commit_unreachable must be terminal for the sweep")
	}
	again := f.v.Recheck(ctx, priorOf(f, r))
	wantReason(t, again, ProvenanceUnverified, ProvReasonCommitUnreachable)
}

// #506 C6 (design 22, "Amendment 2026-10-22 (#506)"): any committed file
// can verify. Authority is the file being on the trusted branch, not its
// directory, so the rule is only that the path is canonical; whether it is a
// regular file there and its bytes match is the verifier's later steps.
// Before #506, AGENTS.md, docs/public pages, plans and skills could never
// verify, a degradation against role-of-record before #482.
func TestIsProvenanceEligiblePath(t *testing.T) {
	for p, want := range map[string]bool{
		ProvenanceLLDDir + "/22-x.md":    true,
		ProvenanceTenetsFile:             true,
		"CLAUDE.md":                      true,
		"AGENTS.md":                      true,
		ProvenanceLLDDir + "/plans/a.md": true,
		"docs/public/guide.md":           true,
		ProvenanceTenetsFile + ".bak":    true,
		"claude.md":                      true,
		"":                               false,
		"/etc/passwd":                    false,
		"docs/../secret.md":              false,
		"docs/./a.md":                    false,
		"docs//a.md":                     false,
		"docs/a.md/":                     false,
		"docs\\a.md":                     false,
		"docs/a\x00.md":                  false,
	} {
		if got := IsProvenanceEligiblePath(p); got != want {
			t.Errorf("%q: got %v want %v", p, got, want)
		}
	}
}

func TestGitBlobIDMatchesGit(t *testing.T) {
	dir := t.TempDir()
	provGit(t, dir, "init", "-q")
	for _, c := range []string{"", "x", provLLDBody, "a\x00b"} {
		want := strings.TrimSpace(provGitStdin(t, dir, c, "hash-object", "--stdin"))
		if got := gitBlobID([]byte(c), false); got != want {
			t.Errorf("%q: %s, git says %s", c, got, want)
		}
	}
}

package spawn

// GitHub #506 (design 22, "Amendment 2026-10-22 (#506)", C1 and review 28a5
// F2): daemon-only provenance clones run under their own kind. A
// provenance root is not a workspace root: GitHTTPBackend (the daemon's git
// serving path for agents) and GitWorkspace never run under it, and the
// provenance kind runs only the verifier's read subcommands, a pinned fetch
// and a pinned clone. Found at: review, observed at integration (an agent
// route reaching the objects the verifier reads would forge provenance).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newProvenanceRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "provenance")
	if err := os.MkdirAll(filepath.Join(root, "github.com", "acme", "widgets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RegisterProvenanceRoot(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unregisterProvenanceRootForTest(root) })
	return root
}

func TestGitProvenance506_ReadsOnlyUnderAProvenanceRoot(t *testing.T) {
	ctx := context.Background()
	root := newProvenanceRoot(t)
	clone := filepath.Join(root, "github.com", "acme", "widgets")

	for _, args := range [][]string{
		{"rev-parse", "--verify", "--quiet", "refs/remotes/origin/main^{commit}"},
		{"remote", "get-url", "origin"},
		{"cat-file", "tree", "abc123"},
		{"rev-list", "--count", "a", "^b"},
		{"fetch", "--quiet", "origin", "+refs/heads/main:refs/remotes/origin/main"},
	} {
		if _, err := GitProvenance(ctx, clone, args...); err != nil {
			t.Errorf("%v refused: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"push", "origin", "main"},
		{"config", "core.hooksPath", "/tmp"},
		{"remote", "add", "x", "https://evil.example/x"},
		{"fetch", "--upload-pack=/bin/sh", "origin"},
		{"fetch", "https://evil.example/x", "+refs/heads/main:refs/remotes/origin/main"},
		{"-c", "core.sshCommand=sh", "rev-parse", "HEAD"},
		{"worktree", "add", "x"},
	} {
		if _, err := GitProvenance(ctx, clone, args...); !errors.Is(err, ErrRefused) {
			t.Errorf("%v was accepted (err %v)", args, err)
		}
	}
	if _, err := GitProvenance(ctx, t.TempDir(), "rev-parse", "HEAD"); !errors.Is(err, ErrRefused) {
		t.Error("a directory outside every provenance root was accepted")
	}
}

func TestGitProvenance506_AProvenanceRootIsNotAWorkspaceRoot(t *testing.T) {
	ctx := context.Background()
	root := newProvenanceRoot(t)
	clone := filepath.Join(root, "github.com", "acme", "widgets")
	if _, err := GitWorkspace(ctx, clone, "rev-parse", "HEAD"); !errors.Is(err, ErrRefused) {
		t.Error("GitWorkspace ran under a provenance root")
	}
	if _, err := GitHTTPBackend(ctx, []string{"GIT_PROJECT_ROOT=" + root, "PATH_INFO=/x"}); !errors.Is(err, ErrRefused) {
		t.Error("GitHTTPBackend served a provenance root: an agent could write the objects the verifier reads")
	}
}

func TestGitProvenanceClone506_PinnedShape(t *testing.T) {
	ctx := context.Background()
	root := newProvenanceRoot(t)
	dest := filepath.Join(root, "github.com", "acme", "gadgets")
	c, err := GitProvenanceClone(ctx, root, "https://github.com/acme/gadgets.git", dest)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(c.Args(), " ")
	for _, want := range []string{"clone", "--no-checkout", "--filter=blob:none", "--no-tags", "https://github.com/acme/gadgets.git", dest} {
		if !strings.Contains(args, want) {
			t.Errorf("clone args %q lack %q", args, want)
		}
	}
	if !strings.Contains(strings.Join(c.Env(), "\n"), "GIT_ALLOW_PROTOCOL=https") {
		t.Error("the clone may use a protocol other than https")
	}
	for _, bad := range []struct{ url, dest string }{
		{"http://github.com/acme/gadgets.git", dest},
		{"https://user:pw@github.com/acme/gadgets.git", dest},
		{"https://github.com:8443/acme/gadgets.git", dest},
		{"ext::sh -c touch% /tmp/x", dest},
		{"https://github.com/acme/gadgets.git", filepath.Join(t.TempDir(), "elsewhere")},
		{"https://github.com/acme/gadgets.git", filepath.Join(root, "..", "escape")},
		{"https://github.com/acme/widgets.git", filepath.Join(root, "github.com", "acme", "widgets")}, // exists
	} {
		if _, err := GitProvenanceClone(ctx, root, bad.url, bad.dest); !errors.Is(err, ErrRefused) {
			t.Errorf("clone %q into %q was accepted (err %v)", bad.url, bad.dest, err)
		}
	}
	if _, err := GitProvenanceClone(ctx, t.TempDir(), "https://github.com/acme/gadgets.git", dest); !errors.Is(err, ErrRefused) {
		t.Error("a clone into an unregistered root was accepted")
	}
}

func TestGitProvenance506_AuthHeaderOnlyOnFetchAndClone(t *testing.T) {
	ctx := context.Background()
	root := newProvenanceRoot(t)
	clone := filepath.Join(root, "github.com", "acme", "widgets")
	f, err := GitProvenance(ctx, clone, "fetch", "--quiet", "origin", "+refs/heads/main:refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.WithGitHTTPAuthHeader("Authorization: Basic eA=="); err != nil {
		t.Fatalf("fetch refused the header: %v", err)
	}
	if strings.Contains(strings.Join(f.Args(), " "), "eA==") {
		t.Fatal("the credential is in argv")
	}
	r, err := GitProvenance(ctx, clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WithGitHTTPAuthHeader("Authorization: Basic eA=="); !errors.Is(err, ErrRefused) {
		t.Fatal("a read accepted a credential")
	}
}

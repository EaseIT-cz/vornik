package service

// GitHub #506 (P1, release blocker), design 22 "Amendment 2026-10-22
// (#506)" C1: zero configuration. A project with no clone configured, serving
// a repository it has never seen, verifies a committed document against an
// automatic daemon-only clone of that repository; its own repository uses its
// configured clone; a refused configured clone is unsafe, never "no clone".
// The runner clones a local origin and points origin at the https URL the
// scope names; every git read the verifier makes then goes through the real
// process-spawn law (the provenance kind). Found at: production (operator
// observation 2026-10-22: no companion design verified out of the box).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/spawn"
)

type localOriginRunner struct{ origin string }

func (r localOriginRunner) Clone(ctx context.Context, _, url, dest, _ string) error {
	if err := exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-checkout", r.origin, dest).Run(); err != nil {
		return err
	}
	return exec.CommandContext(ctx, "git", "-C", dest, "remote", "set-url", "origin", url).Run()
}
func (r localOriginRunner) Fetch(_ context.Context, _, _, _ string) error { return nil }
func (r localOriginRunner) OriginHead(_ context.Context, _ string) (string, error) {
	return "main", nil
}

func gitT(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestProvenanceAutoClones506_ZeroConfigVerifiesACommittedDocument(t *testing.T) {
	ctx := context.Background()
	origin := filepath.Join(t.TempDir(), "origin")
	gitT(t, "init", "--quiet", "-b", "main", origin)
	// Built from the constant: the CE export rewrites a whole design-path
	// literal to the public docs site (see memory.ProvenanceLLDDir).
	design := memory.ProvenanceLLDDir + "/07-x.md"
	body := "# A design\n\nCommitted on main.\n"
	if err := os.MkdirAll(filepath.Join(origin, filepath.Dir(design)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, design), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, "-C", origin, "add", design)
	gitT(t, "-C", origin, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "design")

	base := t.TempDir()
	workspaceRoot := filepath.Join(base, "workspaces")
	autoRoot := provenanceAutoRoot(workspaceRoot)
	if err := os.MkdirAll(autoRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spawn.RegisterProvenanceRoot(autoRoot); err != nil {
		t.Fatal(err)
	}
	auto := &memory.AutoClones{Root: autoRoot, Run: localOriginRunner{origin: origin}}
	v := memory.NewProvenanceVerifier(workspaceRoot)
	projects := map[string]*registry.Project{"companion": {ID: "companion"}} // nothing configured
	project := func(id string) *registry.Project { return projects[id] }
	wireProvenanceTrustRoot(v, workspaceRoot, project)
	wireProvenanceAutoClones(v, workspaceRoot, project, auto)

	res := v.Verify(ctx, memory.ProvenanceRequest{ProjectID: "companion", RepoScope: "github.com/acme/widgets",
		Path: design, Content: []byte(body)})
	if !res.Verified() {
		t.Fatalf("a committed design in an unconfigured project: %+v (want verified)", res)
	}
	if want := filepath.Join(autoRoot, "github.com", "acme", "widgets"); !strings.HasPrefix(res.Repo, "github.com/acme/widgets") || !isDir(want) {
		t.Fatalf("no automatic clone at %s (repo %q)", want, res.Repo)
	}

	// A document that differs from main is examined and fails: unverified,
	// never unavailable.
	res = v.Verify(ctx, memory.ProvenanceRequest{ProjectID: "companion", RepoScope: "github.com/acme/widgets",
		Path: design, Content: []byte("# Forged\n")})
	if res.Verified() || res.Reason != memory.ProvReasonBlobMismatch {
		t.Fatalf("a forged design: %+v (want blob_mismatch)", res)
	}

	// A host off the list gets no clone: clone_failed, the only state that
	// may be stored as unavailable.
	res = v.Verify(ctx, memory.ProvenanceRequest{ProjectID: "companion", RepoScope: "evil.example/acme/widgets",
		Path: design, Content: []byte(body)})
	if res.Reason != memory.ProvReasonCloneFailed {
		t.Fatalf("an unlisted host: %+v (want clone_failed)", res)
	}
}

func TestProvenanceAutoClones506_ConfiguredCloneAndBranchApplyOnlyToTheProjectsOwnScope(t *testing.T) {
	base := t.TempDir()
	workspaceRoot := filepath.Join(base, "workspaces")
	refused := filepath.Join(workspaceRoot, "inside") // under the workspace root: refused
	if err := os.MkdirAll(refused, 0o700); err != nil {
		t.Fatal(err)
	}
	projects := map[string]*registry.Project{"p": {ID: "p", RepoScope: "github.com/acme/own"}}
	projects["p"].Memory.ProvenanceClone = refused
	projects["p"].Memory.ProvenanceBranch = "trunk"
	v := memory.NewProvenanceVerifier(workspaceRoot)
	project := func(id string) *registry.Project { return projects[id] }
	wireProvenanceTrustRoot(v, workspaceRoot, project)
	wireProvenanceAutoClones(v, workspaceRoot, project, nil)

	if _, reason := v.CloneFor(context.Background(), "p", "github.com/acme/own"); reason != memory.ProvReasonUnsafeClone {
		t.Fatalf("a refused configured clone: %q (want unsafe_clone, never no_clone)", reason)
	}
	// A case variant of the project's own scope does not escape its
	// configured clone (security review 2026-10-23).
	if _, reason := v.CloneFor(context.Background(), "p", "github.com/ACME/Own"); reason != memory.ProvReasonUnsafeClone {
		t.Fatalf("a case variant of the own scope: %q (want the configured clone's answer)", reason)
	}
	if got := v.DefaultBranchFor("p", "GitHub.com/acme/own"); got != "trunk" {
		t.Fatalf("a case variant took another branch: %q", got)
	}
	if _, reason := v.CloneFor(context.Background(), "p", "github.com/acme/other"); reason != memory.ProvReasonNoClone {
		t.Fatalf("another scope with no automatic clones: %q", reason)
	}
	if got := v.DefaultBranchFor("p", "github.com/acme/own"); got != "trunk" {
		t.Fatalf("own scope branch = %q, want trunk", got)
	}
	if got := v.DefaultBranchFor("p", "github.com/acme/other"); got != "" {
		t.Fatalf("another scope took the project's branch %q", got)
	}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

package service

// GitHub #499 (operator decision A18; design 22, part 8g): the verifier reads
// a project's daemon-only memory.provenance_clone when one is configured, and
// never falls back to the agent-mounted workspace clone when that clone is
// refused; the trusted branch is memory.provenance_branch, else
// workspace.refresh_branch.

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/registry"
)

func TestProvenanceTrustRoot_DaemonClone(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspaces")
	daemon := filepath.Join(base, "daemon", "proj")
	inside := filepath.Join(root, "other")
	for _, d := range []string{filepath.Join(root, "plain"), filepath.Join(daemon, ".git", "objects"),
		filepath.Join(daemon, ".git", "refs"), inside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(daemon, ".git", "config"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	projects := map[string]*registry.Project{}
	add := func(id, clone, branch, refresh string) {
		p := &registry.Project{ID: id}
		p.Memory.ProvenanceClone, p.Memory.ProvenanceBranch = clone, branch
		p.Workspace.RefreshBranch = refresh
		projects[id] = p
	}
	add("plain", "", "", "trunk")
	add("proj", daemon, "main", "trunk")
	add("other", inside, "", "")
	v := memory.NewProvenanceVerifier(root)
	wireProvenanceTrustRoot(v, root, func(id string) *registry.Project { return projects[id] })

	if got := v.CloneDir("plain"); got != filepath.Join(root, "plain") {
		t.Fatalf("no provenance_clone: clone %q, want the workspace clone", got)
	}
	if got := v.DefaultBranch("plain"); got != "trunk" {
		t.Fatalf("no provenance_branch: branch %q, want refresh_branch", got)
	}
	want, _ := filepath.EvalSymlinks(daemon)
	if got := v.CloneDir("proj"); got != want {
		t.Fatalf("provenance_clone: clone %q, want %q", got, want)
	}
	if got := v.DefaultBranch("proj"); got != "main" {
		t.Fatalf("provenance_branch: branch %q, want main", got)
	}
	if got := v.CloneDir("other"); got != "" {
		t.Fatalf("a provenance_clone inside the workspace root was used (%q), and must not fall back to the workspace clone", got)
	}
	// A project the registry does not hold keeps the as-built workspace
	// clone (which the verifier then finds missing or checks as before).
	if got := v.CloneDir("unknown"); got != filepath.Join(root, "unknown") {
		t.Fatalf("an unknown project: %q", got)
	}
}

package registry

// GitHub #499 (operator decision A18): memory.provenance_clone is validated
// by the strict loader: an absolute path to an existing non-bare clone whose
// origin is the project's repo_scope. The loader reads files only.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCloneConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProvenanceCloneValidation(t *testing.T) {
	good := t.TempDir()
	writeCloneConfig(t, good, "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:acme/widgets.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	other := t.TempDir()
	writeCloneConfig(t, other, "[remote \"origin\"]\n\turl = https://github.com/acme/other.git\n")
	noOrigin := t.TempDir()
	writeCloneConfig(t, noOrigin, "[remote \"upstream\"]\n\turl = https://github.com/acme/widgets.git\n")
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const scope = "github.com/acme/widgets"
	for _, tc := range []struct {
		name, clone, scope, branch, want string
	}{
		{"unset", "", scope, "", ""},
		{"a good clone", good, scope, "", ""},
		{"a good clone and branch", good, scope, "main", ""},
		{"relative path", "vornik-enterprise", scope, "", "absolute"},
		{"missing", filepath.Join(good, "nope"), scope, "", "existing directory"},
		{"bare repository", bare, scope, "", "non-bare"},
		{"another repository", other, scope, "", "not repo_scope"},
		{"no origin", noOrigin, scope, "", "no origin"},
		{"no repo scope", good, "", "", "repo_scope"},
		{"wildcard scope", good, "*", "", "repo_scope"},
		{"branch expression", good, scope, "main~1", "plain branch name"},
	} {
		p := &Project{RepoScope: tc.scope}
		p.Memory.ProvenanceClone, p.Memory.ProvenanceBranch = tc.clone, tc.branch
		err := p.validateProvenanceClone("p.yaml")
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
	p := &Project{}
	p.Workspace.RefreshBranch = "trunk"
	if p.TrustedBranch() != "trunk" {
		t.Fatal("refresh_branch is the fallback trusted branch")
	}
	p.Memory.ProvenanceBranch = "main"
	if p.TrustedBranch() != "main" {
		t.Fatal("provenance_branch wins")
	}
}

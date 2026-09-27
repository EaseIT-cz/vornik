package controlplane

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Named roots — config-apply-journal design §9.2b. The workspace-context write
// could not be journalled because resolveTarget joins every op under one
// ConfigDir while the workspace tree lives outside it. That left a SECOND
// recovery protocol beside the journal, which is two mechanisms for one
// invariant.

func TestResolveTarget_DefaultRootIsUnchanged(t *testing.T) {
	cfg := t.TempDir()
	e := &ApplyEngine{ConfigDir: cfg}

	got, err := e.resolveTarget("configs/swarms/x.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "configs/swarms/x.md"); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestResolveTarget_NamedRootResolvesElsewhere(t *testing.T) {
	cfg, ws := t.TempDir(), t.TempDir()
	e := &ApplyEngine{ConfigDir: cfg, Roots: map[string]string{"workspace": ws}}

	got, err := e.resolveTarget("workspace/proj-1/.autonomy/PROJECT_CONTEXT.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws, "proj-1/.autonomy/PROJECT_CONTEXT.md"); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if strings.HasPrefix(got, cfg) {
		t.Fatal("a named-root op resolved under the config dir")
	}
}

// Containment still applies INSIDE the named root — the guard is the same
// guard, not a second one.
func TestResolveTarget_NamedRootStillRefusesTraversal(t *testing.T) {
	e := &ApplyEngine{ConfigDir: t.TempDir(), Roots: map[string]string{"workspace": t.TempDir()}}

	for _, rel := range []string{
		"workspace/../etc/passwd",
		"workspace/proj/../../escape",
	} {
		if _, err := e.resolveTarget(rel); err == nil {
			t.Fatalf("traversal through a named root was allowed: %q", rel)
		}
	}
}

// A path whose first segment is NOT a configured root resolves under the config
// dir, exactly as before. Otherwise adding a root would silently re-target
// every op that happens to start with that word.
func TestResolveTarget_UnknownFirstSegmentIsAConfigPath(t *testing.T) {
	cfg := t.TempDir()
	e := &ApplyEngine{ConfigDir: cfg, Roots: map[string]string{"workspace": t.TempDir()}}

	got, err := e.resolveTarget("workflows/dev-pipeline.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "workflows/dev-pipeline.md"); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// A root NAME on its own is not a target: it names a directory, and an op that
// writes a root is a bug, not a file.
func TestResolveTarget_BareRootNameIsRefused(t *testing.T) {
	e := &ApplyEngine{ConfigDir: t.TempDir(), Roots: map[string]string{"workspace": t.TempDir()}}
	if _, err := e.resolveTarget("workspace"); err == nil {
		t.Fatal("a bare root name was accepted as a write target")
	}
	if _, err := e.resolveTarget("workspace/"); err == nil {
		t.Fatal("a bare root name with a trailing slash was accepted")
	}
}

// An engine with no Roots behaves exactly as it did before they existed.
func TestResolveTarget_NoRootsConfigured(t *testing.T) {
	cfg := t.TempDir()
	e := &ApplyEngine{ConfigDir: cfg}
	got, err := e.resolveTarget("workspace/proj/file.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "workspace/proj/file.md"); got != want {
		t.Fatalf("want the config-rooted path, got %q", got)
	}
}

// The git http-backend precondition (process-spawn law design §3, round-1 F3;
// found by S1b-2's hooks-writer audit, 2026-09-26): a project repository's
// hooks/ may be written by the daemon's guard installer alone. Before this, any
// approved proposal kind but workspace_context could resolve an op such as
// workspace/<project>/.git/hooks/post-update under the workspace root, and git
// http-backend would then run it on the host at the next push. A .git segment
// is refused in every root, the config tree included.
func TestResolveTarget_RefusesAnyGitDirectorySegment(t *testing.T) {
	e := &ApplyEngine{ConfigDir: t.TempDir(), Roots: map[string]string{"workspace": t.TempDir()}}
	for _, rel := range []string{
		"workspace/proj/.git/hooks/post-update",
		"workspace/proj/.git/config",
		"workspace/proj/sub/.git/hooks/pre-commit",
		"workspace/proj/.GIT/hooks/x",
		".git/hooks/pre-commit",
		"configs/.git/config",
	} {
		if _, err := e.resolveTarget(rel); !errors.Is(err, ErrGitDirPath) {
			t.Errorf("%q: want ErrGitDirPath, got %v", rel, err)
		}
	}
	// .gitignore and friends are files, not the git directory.
	for _, rel := range []string{"workspace/proj/.gitignore", "workspace/proj/.github/workflows/ci.yml", "configs/.gitattributes"} {
		if _, err := e.resolveTarget(rel); err != nil {
			t.Errorf("%q: want accepted, got %v", rel, err)
		}
	}
}

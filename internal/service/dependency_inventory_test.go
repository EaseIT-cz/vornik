package service

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/projectdeps"
	"vornik.io/vornik/internal/registry"
)

// The project_dependencies doctor check had no caller for its inventory
// (dependency provisioning slice 1 was built but never wired, found
// 2026-09-25), so it always said SKIPPED. The inventory lists only projects
// that declare dependencies, in project order, through the shared planner.
func TestDependencyInventory(t *testing.T) {
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"zeta":  {ID: "zeta", Dependencies: []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "requirements.lock"}}},
		"alpha": {ID: "alpha", Dependencies: []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "requirements.lock"}}},
		"plain": {ID: "plain"},
	})
	inv := dependencyInventory(reg, t.TempDir(), t.TempDir())
	if len(inv) != 2 || inv[0].ProjectID != "alpha" || inv[1].ProjectID != "zeta" {
		t.Fatalf("inventory = %+v", inv)
	}
	// No lockfile in the workspace: the plan reports the problem, which the
	// doctor renders as ERROR, rather than silently reporting nothing.
	if len(inv[0].Plans) != 1 || inv[0].Plans[0].Problem == nil {
		t.Fatalf("a missing lockfile must be a planned problem: %+v", inv[0].Plans)
	}
}

// A tree whose completion marker cannot be read is refused by the mount path,
// so the inventory must report it as a problem, never as installed
// (review-20260925-f900 F2).
func TestDependencyInventory_UnreadableMarkerIsAProblem(t *testing.T) {
	ws, cache := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := "pkg==1.0 \\\n    --hash=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	if err := os.WriteFile(filepath.Join(ws, "alpha", "requirements.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "requirements.lock"}}
	store := projectdeps.NewStore(cache)
	key := projectdeps.NewResolver(store, "").Plan(filepath.Join(ws, "alpha"), entries)[0].Key
	if err := os.MkdirAll(store.Path(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Path(key), projectdeps.CompletionMarker), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"alpha": {ID: "alpha", Dependencies: entries}})
	inv := dependencyInventory(reg, ws, cache)
	if len(inv) != 1 || inv[0].Plans[0].Problem == nil {
		t.Fatalf("an unreadable marker must be a problem: %+v", inv)
	}

	// The same lockfile bytes in a task worktree give the SAME key, so the
	// doctor (main checkout) and the mount path (worktree) agree whenever the
	// lockfile is unchanged (review-20260925-f900 F1).
	wt := filepath.Join(t.TempDir(), ".worktrees", "task-1")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "requirements.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := projectdeps.NewResolver(store, "").Plan(wt, entries)[0].Key; got != key {
		t.Fatalf("worktree key %s != main-checkout key %s for identical lockfile bytes", got, key)
	}
}

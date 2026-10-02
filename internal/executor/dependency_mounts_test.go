package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectdeps"
	"vornik.io/vornik/internal/registry"
)

// The daemon only MOUNTS project dependency trees; `vornikctl deps install`
// installs them (project dependency provisioning design §8.2, 2026-09-25).
// Until now nothing wired the mount at all: a project's dependencies: block
// passed validation and gave its agents nothing.

const mountsLock = "pkg==1.0 \\\n    --hash=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"

func depsPlan(t *testing.T, e *Executor) (*persistence.Task, *executionPlan, string) {
	t.Helper()
	ws := t.TempDir()
	e.config.ProjectWorkspacePath = ws
	e.config.DependencyCacheDir = filepath.Join(t.TempDir(), "deps")
	if err := os.MkdirAll(filepath.Join(ws, "p1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "p1", "requirements.lock"), []byte(mountsLock), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := &executionPlan{project: &registry.Project{ID: "p1", Dependencies: []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "requirements.lock"}}}}
	return &persistence.Task{ID: "t1", ProjectID: "p1"}, plan, filepath.Join(ws, "p1")
}

func installFor(t *testing.T, e *Executor, root string, entries []projectdeps.Entry, images ...string) string {
	t.Helper()
	store := projectdeps.NewStore(e.config.DependencyCacheDir)
	key := projectdeps.NewResolver(store, "").Plan(root, entries)[0].Key
	body, err := projectdeps.EncodeMarker(projectdeps.MarkerMeta{Key: key, Images: images})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Path(key), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Path(key), projectdeps.CompletionMarker), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return store.Path(key)
}

func TestDependencyMounts(t *testing.T) {
	e, rt, _, _, _ := setup()
	task, plan, root := depsPlan(t, e)

	if m, err := e.dependencyMounts(task, &executionPlan{project: &registry.Project{ID: "p1"}}, "img:1"); m != nil || err != nil {
		t.Fatalf("no manifest: no mounts and no error, got %v %v", m, err)
	}
	if _, err := e.dependencyMounts(task, plan, "img:1"); !errors.Is(err, projectdeps.ErrNotInstalled) || !strings.Contains(err.Error(), "vornikctl deps install p1") {
		t.Fatalf("not installed: want the install remedy, got %v", err)
	}
	path := installFor(t, e, root, plan.project.Dependencies, "img:1")
	if _, err := e.dependencyMounts(task, plan, "img:2"); !errors.Is(err, projectdeps.ErrInstalledForOtherImage) {
		t.Fatalf("installed for another image: want a refusal, got %v", err)
	}
	mounts, err := e.dependencyMounts(task, plan, "img:1")
	if err != nil || len(mounts) != 1 || mounts[0].HostPath != path {
		t.Fatalf("installed for this image: want its mount, got %+v %v", mounts, err)
	}

	// The mounts reach the container config.
	role := &registry.SwarmRole{Name: "coder", Runtime: registry.SwarmRoleRuntime{Image: "img:1"}}
	if _, err := e.startContainer(context.Background(), task, "e1", "img:1", "coder", "/tmp/in", "/tmp/out", "/tmp/work", role, "", e.config.DefaultTimeout, nil, mounts); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	got := rt.lastConfig.DependencyMounts
	rt.mu.Unlock()
	if len(got) != 1 || got[0].HostPath != path {
		t.Fatalf("container config mounts = %+v", got)
	}

	e.config.DependencyCacheDir = ""
	if _, err := e.dependencyMounts(task, plan, "img:1"); err == nil || !strings.Contains(err.Error(), "dependency_cache_path") {
		t.Fatalf("no cache configured: want a named refusal, got %v", err)
	}
}

func TestPlanDeclaresDependencies(t *testing.T) {
	if planDeclaresDependencies(nil) || planDeclaresDependencies(&executionPlan{}) || planDeclaresDependencies(&executionPlan{project: &registry.Project{}}) {
		t.Fatal("no manifest declares nothing")
	}
	if !planDeclaresDependencies(&executionPlan{project: &registry.Project{Dependencies: []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip}}}}) {
		t.Fatal("a manifest declares dependencies, so warm roles take the ephemeral path")
	}
}

// A warm container is started before its task and cannot carry the mounts, so
// the warm step declines a project that declares dependencies and the caller
// falls through to the ephemeral path (dependency provisioning §8.2).
func TestWarmStepDeclinesAProjectWithDependencies(t *testing.T) {
	e, _, _, _, _ := setup()
	_, plan, _ := depsPlan(t, e)
	role := &registry.SwarmRole{Name: "coder", RuntimePolicy: "warm"}
	_, _, err := e.executeWarmAgentStep(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p1"}, &persistence.Execution{ID: "e1"}, plan, "s1", role, nil, "/tmp/work", 0, time.Now(), nil)
	if !errors.Is(err, errWarmIneligible) {
		t.Fatalf("want errWarmIneligible, got %v", err)
	}
}

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// a dependency tree installed before the move records the legacy image name,
// while the registry now hands the executor the canonical one. The tree still
// mounts; without canonicalising the marker it was refused as installed for
// another image, and the task lost its dependencies.
func TestDependencyMounts_TreeInstalledForLegacyNameMounts(t *testing.T) {
	e, _, _, _, _ := setup()
	task, plan, root := depsPlan(t, e)
	path := installFor(t, e, root, plan.project.Dependencies, "ghcr.io/grinco/vornik-agent:latest")
	mounts, err := e.dependencyMounts(task, plan, "ghcr.io/easeit-cz/vornik-agent:latest")
	if err != nil || len(mounts) != 1 || mounts[0].HostPath != path {
		t.Fatalf("a legacy-named tree must mount for the canonical image: %+v %v", mounts, err)
	}
}

package hostdoctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/projectdeps"
	"vornik.io/vornik/internal/registry"
)

// D5 (onboarding-hardening-design.md, 2026-10-02). On the reference host (uid
// 1001, userns_mode keep-id, run_as_user "") the 2026.10.3 deploy pulled the
// published uid-1000 image and every agent step died "contract mount unusable:
// cannot read /app/input/task.json (running as uid:gid 1000:1000)" — while
// agent_image_uid said OK because the image carries the uid-agnostic label.
// R6: the doctor reports what the agent WILL run as, from the same resolver
// the runtime uses (internal/agentuser), and never trusts the label unprobed.

const d5UID, d5GID = 1001, 1001

type d5Calls struct {
	labelProbes []string // users the label probe ran as
	bakedProbes int
	defaultRuns int
}

// d5Checker is a Checker on a config tree with one swarm image and project p,
// with every podman/host seam injected: no test here shells out.
func d5Checker(t *testing.T, calls *d5Calls) *Checker {
	t.Helper()
	dir := t.TempDir()
	minimalProjectConfigs(t, dir, "") // swarm image vornik-agent:latest, project p
	return &Checker{
		configDir:        dir,
		usernsMode:       "keep-id",
		subuidOKFunc:     func() bool { return true },
		hostIdentityFunc: func() (int, int, bool) { return d5UID, d5GID, true },
		imageLabelsFunc: func(context.Context, string) (map[string]string, error) {
			return map[string]string{agentUIDAgnosticLabel: "1"}, nil
		},
		labelProbeFunc: func(_ context.Context, _ string, user string) (bool, string, error) {
			calls.labelProbes = append(calls.labelProbes, user)
			return true, "", nil
		},
		bakedUIDFunc: func(context.Context, string) (int, error) {
			calls.bakedProbes++
			return d5UID, nil
		},
		defaultNSStartFunc: func(context.Context, string) ([]byte, error) {
			calls.defaultRuns++
			return nil, nil
		},
		// Owned by the daemon when it exists; absent paths error, as os.Stat does.
		ownerUIDFunc: func(p string) (int, error) {
			if _, err := os.Stat(p); err != nil {
				return 0, err
			}
			return d5UID, nil
		},
	}
}

func wantStatus(t *testing.T, got Check, status string, msgParts ...string) {
	t.Helper()
	if got.Status != status {
		t.Fatalf("status = %q, want %q (%s; items %v)", got.Status, status, got.Message, got.Items)
	}
	for _, p := range msgParts {
		if !strings.Contains(got.Message, p) {
			t.Errorf("message %q does not contain %q", got.Message, p)
		}
	}
}

// Row 1: a configured run_as_user is what the agent runs as. No probe.
func TestAgentImageUID_D5_ConfiguredRunAsUserIsOK(t *testing.T) {
	calls := &d5Calls{}
	h := d5Checker(t, calls)
	h.runAsUser = "1001:1001"
	// Review 539e F1: the OK says what it did not examine.
	wantStatus(t, h.checkAgentImageUID(context.Background()), "OK", "runs as 1001:1001 (configured; the image was not probed as that user)")
	if len(calls.labelProbes)+calls.bakedProbes+calls.defaultRuns != 0 {
		t.Fatalf("a configured run_as_user ran a probe: %+v", calls)
	}
}

// The label path: OK only after the probe as the resolved user passes.
func TestAgentImageUID_D5_LabelPathIsVerifiedByProbe(t *testing.T) {
	calls := &d5Calls{}
	h := d5Checker(t, calls)
	wantStatus(t, h.checkAgentImageUID(context.Background()), "OK", "runs as 1001", "verified")
	if len(calls.labelProbes) != 1 || calls.labelProbes[0] != "1001:1001" {
		t.Fatalf("label probe ran as %v, want exactly [1001:1001]", calls.labelProbes)
	}
}

// R4/R6 (review 44bf F3, 4ab9 F1): a lying label — label present, image
// unusable as the daemon uid — is an ERROR from the probe, not OK from the label.
func TestAgentImageUID_D5_LyingLabelIsError(t *testing.T) {
	calls := &d5Calls{}
	h := d5Checker(t, calls)
	h.labelProbeFunc = func(context.Context, string, string) (bool, string, error) {
		return false, "HOME not writable", nil
	}
	wantStatus(t, h.checkAgentImageUID(context.Background()), "ERROR",
		"claims uid-agnostic but does not work as 1001", "runtime.run_as_user")
}

// A label probe that could not complete is NOT MEASURED (SKIPPED, §E2), not a
// verdict on the image.
func TestAgentImageUID_D5_LabelProbeNotMeasuredIsSkipped(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	h.labelProbeFunc = func(context.Context, string, string) (bool, string, error) {
		return false, "", errors.New("signal: killed")
	}
	wantStatus(t, h.checkAgentImageUID(context.Background()), "SKIPPED", "NOT MEASURED")
}

// Unlabelled under keep-id: the baked uid is what runs.
func TestAgentImageUID_D5_UnlabelledKeepID(t *testing.T) {
	calls := &d5Calls{}
	h := d5Checker(t, calls)
	h.imageLabelsFunc = func(context.Context, string) (map[string]string, error) { return map[string]string{}, nil }
	wantStatus(t, h.checkAgentImageUID(context.Background()), "OK")
	if len(calls.labelProbes) != 0 {
		t.Fatal("an unlabelled image ran the label probe")
	}

	h.bakedUIDFunc = func(context.Context, string) (int, error) { return 1000, nil }
	wantStatus(t, h.checkAgentImageUID(context.Background()), "ERROR", `runtime.run_as_user: "1001:1001"`)
}

// userns_mode unset on a host whose default namespace starts: the non-root
// agent maps to a subuid and cannot read the 0700 mounts (measured, D5).
func TestAgentImageUID_D5_UnsetUsernsDefaultStartsIsError(t *testing.T) {
	calls := &d5Calls{}
	h := d5Checker(t, calls)
	h.usernsMode = ""
	wantStatus(t, h.checkAgentImageUID(context.Background()), "ERROR", "runtime.userns_mode: keep-id")
	if calls.defaultRuns != 1 {
		t.Fatalf("default-namespace start ran %d times, want 1", calls.defaultRuns)
	}
	if len(calls.labelProbes) != 0 {
		t.Fatal("the keep-id label probe ran although the runtime would not reach keep-id")
	}
}

// userns_mode unset where the default namespace cannot be set up: the runtime's
// chain reaches keep-id, so the keep-id branches apply (review 4ab9 F2).
func TestAgentImageUID_D5_UnsetUsernsChainReachesKeepID(t *testing.T) {
	calls := &d5Calls{}
	h := d5Checker(t, calls)
	h.usernsMode = ""
	h.defaultNSStartFunc = func(context.Context, string) ([]byte, error) {
		calls.defaultRuns++
		return []byte(`Error: cannot set up namespace using "/usr/bin/newuidmap": exit status 1`), errors.New("exit status 125")
	}
	wantStatus(t, h.checkAgentImageUID(context.Background()), "OK", "verified")
	if len(calls.labelProbes) != 1 || calls.labelProbes[0] != "1001:1001" {
		t.Fatalf("label probe ran as %v, want [1001:1001]", calls.labelProbes)
	}
}

// A default-namespace start that fails for another reason measured nothing.
func TestAgentImageUID_D5_UnsetUsernsOtherFailureIsSkipped(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	h.usernsMode = ""
	h.defaultNSStartFunc = func(context.Context, string) ([]byte, error) {
		return []byte("Error: image not known"), errors.New("exit status 125")
	}
	wantStatus(t, h.checkAgentImageUID(context.Background()), "SKIPPED", "NOT MEASURED")
}

// A configured private namespace is the default namespace with no chain.
func TestAgentImageUID_D5_PrivateUsernsIsError(t *testing.T) {
	calls := &d5Calls{}
	h := d5Checker(t, calls)
	h.usernsMode = "private"
	got := h.checkAgentImageUID(context.Background())
	wantStatus(t, got, "ERROR", "runtime.userns_mode: keep-id")
	// Review 539e F3: reasoned from the measured default namespace, and says so.
	wantStatus(t, got, "ERROR", "not measured for private")
	if calls.defaultRuns != 0 {
		t.Fatal("a configured private namespace has no chain to decide; no start needed")
	}
}

// Rootful, or userns_mode: host: one shared WARNING (review 4ab9 F4).
func TestAgentImageUID_D5_RootfulAndHostShareOneWarning(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	h.hostIdentityFunc = func() (int, int, bool) { return 0, 0, false }
	wantStatus(t, h.checkAgentImageUID(context.Background()), "WARNING",
		"rootful podman: set `runtime.run_as_user`; the keep-id rule does not apply")

	h = d5Checker(t, &d5Calls{})
	h.usernsMode = "host"
	wantStatus(t, h.checkAgentImageUID(context.Background()), "WARNING",
		"userns_mode: host: set `runtime.run_as_user`; the keep-id rule does not apply")
}

// R6 mount owner (review 44bf F2, 4ab9 F3): a project directory, git directory
// or dependency mount whose owner differs from the resolved user is a WARNING
// naming the project and the path; the denominator is published.
func TestAgentImageUID_D5_MountOwnerMismatchWarnsNamingProject(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	ws := t.TempDir()
	h.workspacesRoot = ws
	projDir := filepath.Join(ws, "p")
	if err := os.MkdirAll(filepath.Join(projDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.ownerUIDFunc = func(path string) (int, error) {
		if path == projDir {
			return 4242, nil
		}
		return d5UID, nil
	}

	got := h.checkAgentImageUID(context.Background())

	wantStatus(t, got, "WARNING", "2 mount paths examined")
	joined := strings.Join(got.Items, "\n")
	if !strings.Contains(joined, "project p") || !strings.Contains(joined, projDir) || !strings.Contains(joined, "4242") {
		t.Fatalf("items do not name project p, %s and owner 4242: %v", projDir, got.Items)
	}
	if strings.Contains(joined, filepath.Join(projDir, ".git")) {
		t.Fatalf("the matching git dir was flagged: %v", got.Items)
	}
}

func TestAgentImageUID_D5_MountOwnerGitAndDependencyMounts(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	ws := t.TempDir()
	h.workspacesRoot = ws
	projDir := filepath.Join(ws, "p")
	gitDir := filepath.Join(projDir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	depPath := filepath.Join(t.TempDir(), "pip-key")
	h.dependencyMountsFunc = func(string, string) []string { return []string{depPath} }
	h.ownerUIDFunc = func(path string) (int, error) {
		switch path {
		case gitDir, depPath:
			return 4242, nil
		}
		return d5UID, nil
	}

	got := h.checkAgentImageUID(context.Background())

	wantStatus(t, got, "WARNING", "3 mount paths examined")
	joined := strings.Join(got.Items, "\n")
	for _, want := range []string{gitDir, depPath} {
		if !strings.Contains(joined, want) {
			t.Errorf("items do not name %s: %v", want, got.Items)
		}
	}
}

// Matching owners leave the verdict OK and still say how much was examined.
func TestAgentImageUID_D5_MountOwnersMatchStayOK(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	ws := t.TempDir()
	h.workspacesRoot = ws
	if err := os.MkdirAll(filepath.Join(ws, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.checkAgentImageUID(context.Background()), "OK", "1 mount path examined")
}

// A configured run_as_user is compared by its uid.
func TestAgentImageUID_D5_MountOwnerComparedWithConfiguredUser(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	h.runAsUser = "1000:1000"
	ws := t.TempDir()
	h.workspacesRoot = ws
	if err := os.MkdirAll(filepath.Join(ws, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := h.checkAgentImageUID(context.Background())
	wantStatus(t, got, "WARNING", "runs as 1000:1000 (configured;")
}

func TestUserUID(t *testing.T) {
	for in, want := range map[string]int{"1001": 1001, "1000:1000": 1000, "0:0": 0, "root:root": 0} {
		got, err := userUID(in)
		if err != nil || got != want {
			t.Errorf("userUID(%q) = (%d, %v), want %d", in, got, err, want)
		}
	}
	if _, err := userUID("no-such-user-d5:x"); err == nil {
		t.Error("userUID of an unknown name: want an error")
	}
}

// An unresolvable configured user says mount owners were NOT EXAMINED rather
// than reporting a clean comparison that never happened.
func TestAgentImageUID_D5_UnresolvableRunAsUserSaysNotExamined(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	h.runAsUser = "no-such-user-d5"
	h.workspacesRoot = t.TempDir()
	wantStatus(t, h.checkAgentImageUID(context.Background()), "OK", "NOT EXAMINED")
}

func TestAgentImageUID_D5_NoWorkspaceSaysNotExamined(t *testing.T) {
	h := d5Checker(t, &d5Calls{})
	wantStatus(t, h.checkAgentImageUID(context.Background()), "OK", "mount owners NOT EXAMINED")
}

func TestRealOwnerUID(t *testing.T) {
	dir := t.TempDir()
	if got, err := realOwnerUID(dir); err != nil || got != os.Getuid() {
		t.Fatalf("realOwnerUID(own temp dir) = (%d, %v), want %d", got, err, os.Getuid())
	}
	if _, err := realOwnerUID(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("realOwnerUID of an absent path: want an error")
	}
}

func TestWithRuntime(t *testing.T) {
	h := New("/c", "keep-id", "").WithRuntime(RuntimeEnv{RunAsUser: " 1:1 ", ProjectWorkspacePath: "/w", DependencyCacheDir: "/d"})
	if h.runAsUser != "1:1" || h.workspacesRoot != "/w" || h.depsCacheDir != "/d" {
		t.Fatalf("WithRuntime did not set the fields: %+v", h)
	}
}

// The real dependency path planning: materialised entries become mount paths
// under the cache, exactly as projectdeps.Plan.Mount gives the executor.
func TestDependencyMountPaths_PlansLikeTheExecutor(t *testing.T) {
	cache := t.TempDir()
	projDir := t.TempDir()
	h := &Checker{depsCacheDir: cache}
	p := &registry.Project{ID: "p"}
	if got := h.dependencyMountPaths(p, projDir); got != nil {
		t.Fatalf("a project with no dependencies has mounts: %v", got)
	}
	p.Dependencies = []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "requirements.lock"}}
	if got := h.dependencyMountPaths(p, projDir); len(got) != 0 {
		t.Fatalf("an unmaterialised dependency has mounts: %v", got)
	}
	if got := (&Checker{}).dependencyMountPaths(p, projDir); got != nil {
		t.Fatalf("no cache dir configured yet mounts: %v", got)
	}
}

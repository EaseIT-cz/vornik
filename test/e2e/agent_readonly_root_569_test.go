//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/runtime"
)

// GitHub #569 (08-runtime-manager-podman.md, "Change 2026-10-28"), e2e lane:
// the pinned agent image under the argv the runtime builds with the read-only
// root on. Found at: review (SaaS MVP design §1.5, review-20261012-1370); a
// user would observe it at e2e-lane, which is where this sits.

func hardenedManager569(t *testing.T, extra ...runtime.ManagerOption) *runtime.Manager {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("requires Linux + podman")
	}
	requirePodman(t)
	if out, err := exec.Command("podman", "pull", publishedAgentImageD5).CombinedOutput(); err != nil {
		t.Fatalf("podman pull %s: %v\n%s", publishedAgentImageD5, err, out)
	}
	opts := append([]runtime.ManagerOption{runtime.WithAgentReadOnlyRoot(true), runtime.WithAgentPidsLimit(2048)}, extra...)
	m, err := runtime.New(opts...)
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	return m
}

func stamp569() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

// probe569 runs the runtime's argv with the entrypoint swapped for sh -c.
func probe569(t *testing.T, ctx context.Context, args []string, script string) string {
	t.Helper()
	probe := make([]string, 0, len(args)+6)
	for _, a := range args[:len(args)-1] {
		if a == "--detach" {
			a = "--rm"
		}
		probe = append(probe, a)
	}
	probe = append(probe, "--entrypoint", "sh", args[len(args)-1], "-c", script)
	out, err := exec.CommandContext(ctx, "podman", probe...).CombinedOutput()
	if err != nil {
		t.Fatalf("probe under the runtime's argv: %v\n%s", err, out)
	}
	return string(out)
}

func TestAgentReadOnlyRoot569_WritesAndRefusals(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "vornik.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := hardenedManager569(t, runtime.WithDaemonSocketPath(sock))
	ctx, cancel := context.WithTimeout(context.Background(), agentRunTimeout)
	defer cancel()
	args, err := m.RunArgsForTest(ctx, &runtime.ContainerConfig{
		Image: publishedAgentImageD5, ProjectID: "e2e-569", Role: "coder", TaskID: "ro-" + stamp569(),
		InputDir: t.TempDir(), WorkspaceDir: t.TempDir(), Network: runtime.NetworkDaemonOnly,
	}, "keep-id") // the attempt the agent writes its workspace under (D5's --user)
	if err != nil {
		t.Fatalf("RunArgsForTest: %v", err)
	}
	out := probe569(t, ctx, args, `
for p in /app/workspace/w /tmp/w /var/tmp/w /home/vornik/.cache/w /run/lock/w /dev/shm/w; do ( : > "$p" ) 2>/dev/null && echo "W $p" || echo "- $p"; done
for p in /entrypoint.sh /tool_registry.generated.sh /app/x /usr/local/x; do ( : > "$p" ) 2>/dev/null && echo "W $p" || echo "- $p"; done
df -P /tmp /var/tmp /home/vornik | awk 'NR>1{print "fs", $6, $1}'
df -Pk /run /dev/shm | awk 'NR>1{print "size", $6, $2}'
[ -e /run/vornik/vornik.sock ] && echo "socket present"
cat /sys/fs/cgroup/pids.max`)
	for _, want := range []string{
		"W /app/workspace/w", "W /tmp/w", "W /var/tmp/w", "W /home/vornik/.cache/w", "W /run/lock/w", "W /dev/shm/w",
		"- /entrypoint.sh", "- /tool_registry.generated.sh", "- /app/x", "- /usr/local/x",
		"size /run 16384", "size /dev/shm 65536", "socket present", "2048",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	// The --read-only-tmpfs=false guard: the volumes, not podman's tmpfs.
	for _, dir := range []string{"/tmp", "/var/tmp", "/home/vornik"} {
		if strings.Contains(out, "fs "+dir+" tmpfs") {
			t.Errorf("%s is a tmpfs, not its disk-backed volume:\n%s", dir, out)
		}
	}
}

// volumesByDestination reads a container's anonymous volumes from podman.
func volumesByDestination(t *testing.T, id string) map[string]string {
	t.Helper()
	out, err := exec.Command("podman", "inspect", "--format", "{{json .Mounts}}", id).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect %s: %v\n%s", id, err, out)
	}
	var mounts []struct{ Type, Name, Destination string }
	if err := json.Unmarshal(out, &mounts); err != nil {
		t.Fatalf("mounts of %s: %v\n%s", id, err, out)
	}
	got := map[string]string{}
	for _, mt := range mounts {
		if mt.Type == "volume" {
			got[mt.Destination] = mt.Name
		}
	}
	for _, d := range []string{"/tmp", "/var/tmp", "/home/vornik"} {
		if got[d] == "" {
			t.Fatalf("%s has no anonymous volume at %s: %v", id, d, got)
		}
	}
	return got
}

func assertVolumesGone(t *testing.T, vols map[string]string, when string) {
	t.Helper()
	for dest, name := range vols {
		if err := exec.Command("podman", "volume", "exists", name).Run(); err == nil {
			t.Errorf("%s: the %s volume %s is left behind", when, dest, name)
		}
	}
}

func startWaitExited569(t *testing.T, ctx context.Context, m *runtime.Manager, cfg *runtime.ContainerConfig) string {
	t.Helper()
	id, err := m.StartContainer(ctx, cfg)
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	// The entrypoint finds no task and exits; wait for it.
	_, _ = exec.CommandContext(ctx, "podman", "wait", id).CombinedOutput()
	return id
}

func TestAgentReadOnlyRoot569_VolumesGoWithTheContainer(t *testing.T) {
	m := hardenedManager569(t)
	ctx, cancel := context.WithTimeout(context.Background(), agentRunTimeout)
	defer cancel()
	cfg := &runtime.ContainerConfig{Image: publishedAgentImageD5, ProjectID: "e2e-569", Role: "coder",
		TaskID: "vol-" + stamp569(), InputDir: t.TempDir(), OutputDir: t.TempDir()}

	first := startWaitExited569(t, ctx, m, cfg)
	firstVols := volumesByDestination(t, first)
	// A second start of the same name removes the first, its volumes too.
	second := startWaitExited569(t, ctx, m, cfg)
	assertVolumesGone(t, firstVols, "a second start of the same name")
	secondVols := volumesByDestination(t, second)
	if err := m.RemoveContainer(ctx, second, true); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	assertVolumesGone(t, secondVols, "RemoveContainer")

	// An exited container of a terminal task is swept, volumes included.
	cfg.TaskID = "sweep-" + stamp569()
	third := startWaitExited569(t, ctx, m, cfg)
	thirdVols := volumesByDestination(t, third)
	mine := cfg.TaskID
	lookup := func(_ context.Context, id string) (runtime.OrphanTask, error) {
		if id == mine {
			return runtime.OrphanTask{Found: true, Terminal: true, UpdatedAt: time.Now().Add(-time.Hour)}, nil
		}
		return runtime.OrphanTask{}, nil // every other container on this host: unknown, kept
	}
	m.SweepOrphans(ctx, lookup, time.Now())
	if err := exec.Command("podman", "container", "exists", third).Run(); err == nil {
		t.Error("the sweep left the exited container of a terminal task")
		_ = m.RemoveContainer(ctx, third, true)
	}
	assertVolumesGone(t, thirdVols, "the sweep")
}

func TestAgentReadOnlyRoot569_WarmContainerSurvivesTheSweep(t *testing.T) {
	m := hardenedManager569(t)
	ctx, cancel := context.WithTimeout(context.Background(), agentRunTimeout)
	defer cancel()
	cfg := &runtime.ContainerConfig{Image: publishedAgentImageD5, ProjectID: "e2e-569", Role: "coder",
		TaskID: "warm-" + strconv.FormatInt(time.Now().UnixNano(), 10), InputDir: t.TempDir(), OutputDir: t.TempDir()}
	id := startWaitExited569(t, ctx, m, cfg)
	defer func() { _ = m.RemoveContainer(context.Background(), id, true) }()
	var looked []string
	// Unknown for everything: this sweep runs against the host's real podman,
	// where other daemons' containers live, so nothing here may answer
	// "terminal" for a task it does not own.
	lookup := func(_ context.Context, taskID string) (runtime.OrphanTask, error) {
		looked = append(looked, taskID)
		return runtime.OrphanTask{}, nil
	}
	m.SweepOrphans(ctx, lookup, time.Now())
	m.SweepOrphans(ctx, lookup, time.Now())
	if err := exec.Command("podman", "container", "exists", id).Run(); err != nil {
		t.Fatal("the sweep removed a warm container")
	}
	for _, l := range looked {
		if l == cfg.TaskID {
			t.Fatal("the sweep looked up a warm container's task id")
		}
	}
}

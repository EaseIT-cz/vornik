//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/runtime"
)

// TestAgentContainerHasNoSwap is GitHub #379's e2e-lane check (found at:
// review, 2026.10.6 delta audit #334; observed by a user as host swap filled
// by agent containers, the 2026-10-14 OOM). runtime.Manager emitted --memory
// without --memory-swap, so podman set memory.swap.max to the limit and an
// agent could push a whole limit into host swap before its OOM kill.
//
// It starts the pinned agent image with exactly the argv the runtime builds
// for a 256 MiB limit (RunArgsForTest; the entrypoint swapped for cat, detach
// for --rm) and reads the container's own cgroup v2 files: memory.max must be
// the limit and memory.swap.max must be 0. Before the fix memory.swap.max
// read the limit.
func TestAgentContainerHasNoSwap(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("requires Linux + podman")
	}
	requirePodman(t)
	const limit int64 = 256 * 1024 * 1024

	if out, err := exec.Command("podman", "pull", publishedAgentImageD5).CombinedOutput(); err != nil {
		t.Fatalf("podman pull %s: %v\n%s", publishedAgentImageD5, err, out)
	}
	m, err := runtime.New(runtime.WithAgentMemoryLimit(limit))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	scratch := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), agentRunTimeout)
	defer cancel()
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	args, err := m.RunArgsForTest(ctx, &runtime.ContainerConfig{
		Image: publishedAgentImageD5, ProjectID: "e2e-379", Role: "coder", TaskID: "noswap-" + stamp,
		InputDir: scratch, EnvVars: map[string]string{"VORNIK_LLM_MODEL": "noop"},
	}, "")
	if err != nil {
		t.Fatalf("RunArgsForTest: %v", err)
	}
	probe := make([]string, 0, len(args)+6)
	for _, a := range args[:len(args)-1] {
		if a == "--detach" {
			a = "--rm"
		}
		probe = append(probe, a)
	}
	probe = append(probe, "--entrypoint", "cat", args[len(args)-1],
		"/proc/self/cgroup", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.swap.max")
	out, err := exec.CommandContext(ctx, "podman", probe...).CombinedOutput()
	if err != nil {
		t.Fatalf("cat cgroup files under the runtime's argv: %v\n%s", err, out)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 3 || lines[0] != "0::/" {
		// Not a private cgroup v2 namespace: the files would be another
		// cgroup's, so the check cannot be made. FAIL, never skip (§6).
		t.Fatalf("expected a private cgroup v2 namespace (0::/) and two values, got %q (euid %d)", out, os.Geteuid())
	}
	if lines[1] != strconv.FormatInt(limit, 10) {
		t.Fatalf("memory.max = %s, want the applied limit %d", lines[1], limit)
	}
	if lines[2] != "0" {
		t.Fatalf("memory.swap.max = %s, want 0: the agent container may swap (#379)", lines[2])
	}
}

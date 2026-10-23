//go:build integration

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/projectdeps/install"
)

// GitHub #428 (found at: review; the level an operator meets it is a real
// `vornikctl deps install`): with real podman and the real agent image, the
// install's memory bound reaches the container. A bound install that fits
// succeeds (the flags are accepted by this runtime, so no new start failure),
// and a pip run under a bound too small for pip to start is killed and
// reported as the limit (project dependency provisioning design §8.6).
// Skipped when podman or the agent image is not present on this host.

func TestIntegrationDepsInstall_BoundedInstallSucceeds(t *testing.T) {
	img := agentImage(t)
	wheels := t.TempDir()
	sum := tinyWheel(t, wheels)
	in := integrationInput(t, img, "vorniktiny==1.0 \\\n    --hash=sha256:"+sum+"\n")
	in.Wheelhouse = wheels
	in.MemoryLimit = 512 << 20
	in.MemoryNote = "memory limit 512 MiB, no swap (runtime.agent_memory_limit)"
	var out bytes.Buffer
	if err := runDepsInstall(context.Background(), in, execRunner, &out); err != nil {
		t.Fatalf("a bounded install that fits must succeed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), in.MemoryNote) || !strings.Contains(out.String(), "installed") {
		t.Errorf("want the bound and the install reported:\n%s", out.String())
	}
}

func TestIntegrationDepsInstall_KilledAtTheLimitSaysSo(t *testing.T) {
	img := agentImage(t)
	wheels := t.TempDir()
	sum := tinyWheel(t, wheels)
	in := integrationInput(t, img, "vorniktiny==1.0 \\\n    --hash=sha256:"+sum+"\n")
	// 16 MiB: enough for podman to start the container, too little for
	// python3 -m pip to import, so the cgroup OOM killer ends it.
	const tooSmall = 16 << 20
	fetch := install.PipFetcher(execRunner, img, filepath.Join(in.ProjectRoot, "requirements.lock"), wheels, tooSmall)
	err := fetch(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("pip under a 16 MiB bound must not succeed")
	}
	for _, want := range []string{"killed (exit 137)", "memory limit of 16 MiB (16777216 bytes", "runtime.agent_memory_limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a pip killed at the limit must say %q, got: %v", want, err)
		}
	}
}

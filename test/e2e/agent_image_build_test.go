//go:build e2e
// +build e2e

package e2e_test

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
)

var (
	agentImageBuildOnce sync.Once
	agentImageBuildErr  error
	agentImageBuildOut  []byte
)

// buildAgentImage builds images/vornik-agent as agentImageTag, once per test
// process, with the current uid/gid so bind mounts work under keep-id — this
// mirrors what `make build-agent` does for a local operator, and is
// deterministic across developers and CI because it never hardcodes 1000.
// Every e2e test that needs the real agent image shares this one build.
func buildAgentImage(t *testing.T) string {
	t.Helper()
	requirePodman(t)
	repoRoot := findRepoRoot(t)
	agentImageBuildOnce.Do(func() {
		cmd := exec.Command(
			"podman", "build",
			"-f", "images/vornik-agent/Containerfile",
			"--build-arg", fmt.Sprintf("VORNIK_UID=%d", os.Getuid()),
			"--build-arg", fmt.Sprintf("VORNIK_GID=%d", os.Getgid()),
			"-t", agentImageTag,
			".",
		)
		cmd.Dir = repoRoot
		agentImageBuildOut, agentImageBuildErr = cmd.CombinedOutput()
	})
	if agentImageBuildErr != nil {
		t.Fatalf("podman build failed: %v\n%s", agentImageBuildErr, agentImageBuildOut)
	}
	return agentImageTag
}

package spawn

import (
	"context"
	"maps"
	"slices"
	"testing"
)

// GitHub #569 (08-runtime-manager-podman.md, "Change 2026-10-28", R7): the
// agent's read-only root needs --read-only-tmpfs=false (podman's own tmpfs
// would cover the anonymous volumes) and rm --volumes (so removing a container
// removes its volumes). Each is admitted exactly; nothing else is.

func TestPodmanAgent569_ReadOnlyTmpfsFalseOnly(t *testing.T) {
	ctx := context.Background()
	hardened := []string{"run", "--detach", "--replace", "--name", "vornik-p-r-t",
		"--read-only", "--read-only-tmpfs=false",
		"--tmpfs", "/run:rw,size=16m,mode=1777", "--tmpfs", "/dev/shm:rw,size=64m,mode=1777",
		"--volume", "/tmp", "--volume", "/var/tmp", "--volume", "/home/vornik",
		"--pids-limit", "2048", pinned}
	must(t, "hardened agent start")(PodmanAgent(ctx, "", hardened))
	for _, bad := range [][]string{
		{"run", "--read-only-tmpfs=true", pinned},
		{"run", "--read-only-tmpfs", pinned},
		{"run", "--read-only-tmpfs=yes", pinned},
	} {
		c, err := PodmanAgent(ctx, "", bad)
		refused(t, c, err, bad[1])
	}
}

func TestPodmanControl569_RmVolumes(t *testing.T) {
	ctx := context.Background()
	for _, argv := range [][]string{
		{"rm", "--force", "--volumes", "vornik-p-r-t"},
		{"rm", "-f", "-v", "vornik-p-r-t"},
	} {
		must(t, "rm with volumes")(PodmanControl(ctx, "", argv))
	}
}

// The closed grammar: the flag sets are the previous ones plus these two
// additions, so nothing else slipped in with them.
func TestSpawn569_GrammarIsClosed(t *testing.T) {
	wantAgent := []string{
		"--cap-drop", "--cpu-quota", "--cpus", "--detach", "--entrypoint", "--env", "--interactive",
		"--label", "--memory", "--memory-swap", "--name", "--network", "--pids-limit", "--pull",
		"--read-only", "--read-only-tmpfs", "--replace", "--rm", "--security-opt", "--tmpfs",
		"--ulimit", "--user", "--userns", "--volume", "--workdir", "-d", "-e", "-i", "-v", "-w",
	}
	if got := slices.Sorted(maps.Keys(agentFlags)); !slices.Equal(got, wantAgent) {
		t.Errorf("PodmanAgent flags = %v, want %v", got, wantAgent)
	}
	if got := agentFlags["--read-only-tmpfs"].allowed; len(got) != 1 || !got["false"] {
		t.Errorf("--read-only-tmpfs allows %v, want only false", got)
	}
	wantRm := []string{"--force", "--ignore", "--volumes", "-f", "-i", "-v"}
	if got := slices.Sorted(maps.Keys(controlVerbs["rm"].flags)); !slices.Equal(got, wantRm) {
		t.Errorf("rm flags = %v, want %v", got, wantRm)
	}
}

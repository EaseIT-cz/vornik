package runtime

// Trust-zone invariant TZ-3 (https://docs.vornik.io
// §11, GitHub #338): an agent sandbox never reaches artifact storage — or any
// other daemon-owned host path — directly. The runtime half of that rule is
// pinned here: the podman argv mounts exactly the host paths the caller
// declared in ContainerConfig (input, output, workspace, project dir, .git,
// dependency trees) plus, for daemon-only roles, the daemon socket. A mount
// the runtime adds on its own (the data dir, the artifact store, a secrets
// directory) fails this test. The executor fills the declared fields from a
// per-execution MkdirTemp root and the task worktree (container.go, the
// ContainerConfig literal); that half is read from the code, not pinned here.
//
// audit 2026-10-12 (w3-5): before this test the rule was stated in the
// reference architecture but no test failed when it broke.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/projectdeps"
)

// volumeSources returns the host side of every --volume / -v in args, and
// the count of volume flags examined (the denominator).
func volumeSources(t *testing.T, args []string) []string {
	t.Helper()
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		var spec string
		switch {
		case a == "--volume" || a == "-v":
			if i+1 >= len(args) {
				t.Fatalf("volume flag without a value in %q", args)
			}
			spec = args[i+1]
			i++
		case strings.HasPrefix(a, "--volume="):
			spec = strings.TrimPrefix(a, "--volume=")
		case strings.HasPrefix(a, "-v="):
			spec = strings.TrimPrefix(a, "-v=")
		case a == "--mount" || strings.HasPrefix(a, "--mount="):
			t.Fatalf("--mount is not a form the runtime emits; got %q", args)
		default:
			continue
		}
		src, _, _ := strings.Cut(spec, ":")
		out = append(out, src)
	}
	return out
}

// m0args is a mount-free argv prefix for the extractor self-test.
func m0args(t *testing.T) []string {
	t.Helper()
	return []string{"run", "--detach", "--name", "x", "--env", "A=B:C"}
}

func TestTrustZone_AgentMountsAreExactlyTheDeclaredSet(t *testing.T) {
	const sock = "/run/host/vornik.sock"
	cfg := &ContainerConfig{
		Image:         "alpine:latest",
		ProjectID:     "proj",
		Role:          "coder",
		TaskID:        "task-tz3",
		InputDir:      "/tmp/vornik-exec-1/input",
		OutputDir:     "/tmp/vornik-exec-1/output",
		WorkspaceDir:  "/tmp/vornik-exec-1/workspace",
		ProjectDir:    "/var/lib/vornik/workspaces/proj/.worktrees/task-tz3",
		ProjectGitDir: "/var/lib/vornik/workspaces/proj/.git",
		DependencyMounts: []projectdeps.Mount{
			{Ecosystem: projectdeps.EcosystemPip, HostPath: "/var/lib/vornik/deps/pip/abc"},
		},
	}
	declared := map[string]bool{
		cfg.InputDir: true, cfg.OutputDir: true, cfg.WorkspaceDir: true,
		cfg.ProjectDir: true, cfg.ProjectGitDir: true,
		cfg.DependencyMounts[0].HostPath: true,
	}

	for _, network := range []NetworkMode{NetworkEgress, NetworkNone, NetworkDaemonOnly} {
		cfg.Network = network
		m := &Manager{daemonSocketPath: sock}
		srcs := volumeSources(t, m.buildPreImageArgs(cfg))
		want := len(declared)
		allowed := declared
		if network == NetworkDaemonOnly {
			want++
			allowed = map[string]bool{sock: true}
			for k := range declared {
				allowed[k] = true
			}
		}
		t.Logf("network=%s: %d volume flags examined", network, len(srcs))
		if len(srcs) != want {
			t.Errorf("network=%s: %d mounts, want exactly %d (declared set): %v", network, len(srcs), want, srcs)
		}
		for _, s := range srcs {
			if !allowed[s] {
				t.Errorf("network=%s: mount source %q is not a declared ContainerConfig path", network, s)
			}
		}
	}

	// Self-test: the extractor sees a planted mount in every spelling the
	// grammar admits, so a clean result above is not a walk that saw nothing.
	planted := append(m0args(t), "--volume", "/var/lib/vornik/artifacts:/a:ro", "-v", "/srv/b:/b", "--volume=/srv/c:/c")
	if got := volumeSources(t, planted); len(got) != 3 || got[0] != "/var/lib/vornik/artifacts" || got[1] != "/srv/b" || got[2] != "/srv/c" {
		t.Errorf("self-test: extractor missed a planted mount: %v", got)
	}

	// A config that declares nothing gets no mount at all: the runtime has
	// no default mount of its own.
	m := &Manager{daemonSocketPath: sock}
	if srcs := volumeSources(t, m.buildPreImageArgs(&ContainerConfig{
		Image: "alpine:latest", ProjectID: "proj", Role: "r", TaskID: "t", Network: NetworkEgress,
	})); len(srcs) != 0 {
		t.Errorf("an empty config mounted %v; the runtime must add no mount of its own", srcs)
	}
}

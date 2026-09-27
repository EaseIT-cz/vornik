package runtime

// Process-spawn law, S6 (https://docs.vornik.io,
// "S6 — git state an agent can write").
//
// Two mount rules the S6 git hardening relies on:
//   - S6-D5: a warm container outlives any one task, so it cannot carry a
//     task's worktree; it gets NO /app/workspace/project mount at all. Before S6
//     it always mounted the project directory read-write, .git included: way 4
//     of the design's inventory, an agent-writable .git/hooks and .git/config
//     that the daemon's next git command in the project would run.
//   - The read-only .git mount is a CONTROL: in worktree mode the project's
//     .git is mounted at its host path so the agent's own git resolves the
//     worktree, and it must be read-only, because S6-D2 (the daemon names the
//     admin dir itself) is sound only while an agent cannot write the admin
//     dir, config or hooks.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// volumeSpecs returns every --volume/-v value in argv.
func volumeSpecs(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		switch {
		case (argv[i] == "--volume" || argv[i] == "-v") && i+1 < len(argv):
			out = append(out, argv[i+1])
			i++
		case strings.HasPrefix(argv[i], "--volume="):
			out = append(out, strings.TrimPrefix(argv[i], "--volume="))
		}
	}
	return out
}

// gitDirMountViolations is the :ro law's judgement: every volume whose source
// is gitDir or lies beneath it must be mounted read-only. Separated from argv
// construction so the self-test can prove it fails on a read-write mount.
func gitDirMountViolations(argv []string, gitDir string) []string {
	var bad []string
	for _, spec := range volumeSpecs(argv) {
		parts := strings.SplitN(spec, ":", 3)
		if len(parts) < 2 {
			continue
		}
		src := filepath.Clean(parts[0])
		if src != filepath.Clean(gitDir) && !strings.HasPrefix(src, filepath.Clean(gitDir)+string(filepath.Separator)) {
			continue
		}
		mode := ""
		if len(parts) == 3 {
			mode = parts[2]
		}
		ro := false
		for _, opt := range strings.Split(mode, ",") {
			if opt == "ro" {
				ro = true
			}
		}
		if !ro {
			bad = append(bad, spec)
		}
	}
	return bad
}

func TestS6Law_GitDirMountIsReadOnly_SelfTest(t *testing.T) {
	argv := []string{"--volume", "/p/.git:/p/.git:rw,z", "-v", "/p/.git/hooks:/h", "--volume", "/p/.git:/p/.git:ro,z"}
	got := gitDirMountViolations(argv, "/p/.git")
	if len(got) != 2 {
		t.Fatalf("self-test: the law must flag the rw and the mode-less mount, got %q", got)
	}
}

// THE LAW. In worktree mode the runtime mounts the project's .git, and only
// read-only. Making it read-write (tried before, to let agents commit)
// reopens S6 way 2: an agent-written admin dir the daemon's git would follow.
func TestS6Law_GitDirMountIsReadOnly(t *testing.T) {
	m := &Manager{}
	gitDir := "/host/projects/acme/.git"
	args := m.buildPreImageArgs(&ContainerConfig{
		Image:         "localhost/vornik-agent:test",
		ProjectID:     "acme",
		Role:          "coder",
		TaskID:        "task-ro",
		ProjectDir:    "/host/projects/acme/.worktrees/task-ro",
		ProjectGitDir: gitDir,
	})
	examined := 0
	for _, spec := range volumeSpecs(args) {
		if strings.HasPrefix(spec, gitDir) {
			examined++
		}
	}
	if examined == 0 {
		t.Fatalf("no .git mount emitted at all (examined %d volumes): %q", len(volumeSpecs(args)), args)
	}
	if bad := gitDirMountViolations(args, gitDir); len(bad) != 0 {
		t.Fatalf("the project's .git must be mounted read-only; read-write mounts: %q", bad)
	}
	t.Logf("examined %d volume(s), %d naming the .git dir", len(volumeSpecs(args)), examined)
}

// S6-D5: a warm container's argv carries no project mount, however the pool is
// configured.
func TestS6_WarmContainerMountsNoProjectDirectory(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "argv")
	mgr := &Manager{
		podmanPath: writeFakePodman(t, `#!/usr/bin/env bash
printf '%s\n' "$@" >> "`+logFile+`"
case "$1" in
  run)     echo "warm-s6"; exit 0 ;;
  inspect) echo '[{"Id":"warm-s6","Name":"/w","Image":"img","State":{"Status":"running","ExitCode":0},"Config":{"Labels":{}}}]'; exit 0 ;;
  *)       exit 0 ;;
esac
`),
		logger: zerolog.Nop(),
	}
	ws := t.TempDir()
	pool := newS6WarmPool(mgr, ws)
	key := PoolKey{ProjectID: "p1", Role: "coder", Image: "localhost/vornik-agent:test"}

	entry, err := pool.StartWarm(context.Background(), key, nil)
	if err != nil {
		t.Fatalf("StartWarm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(entry.InputDir)) })

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("fake podman recorded nothing: %v", err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(argv) == 0 || argv[0] != "run" {
		t.Fatalf("expected a podman run, got %q", argv)
	}
	for _, spec := range volumeSpecs(argv) {
		if strings.Contains(spec, ":/app/workspace/project") || strings.HasPrefix(spec, filepath.Join(ws, "p1")) {
			t.Errorf("a warm container must mount no project directory, got volume %q", spec)
		}
	}
	// (Before S6 the entry also carried the project directory; PoolEntry no
	// longer has the field.)
}

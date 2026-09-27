package github

// S6 compatibility matrix, "forge fetch/push over SSH" (process-spawn law
// design, S6): the change-request push to an SSH remote goes through the
// operator's core.sshCommand, which the daemon's composed git config keeps
// (G3). Written and run green before S6 changed production code.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// s6DaemonGitHome writes gitconfig as the operator's global config in a fresh
// HOME and composes the daemon's git config from it, as the daemon does at
// startup (S6-D4).
func s6DaemonGitHome(t *testing.T, gitconfig string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	s6ComposeDaemonGitConfig(t, home)
	return home
}

func TestS6Compat_ForgePushOverSSHUsesOperatorSSHCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	clone := filepath.Join(root, "clone")
	run(t, root, "git", "init", "-q", "--bare", "-b", "main", bare)
	run(t, root, "git", "clone", "-q", bare, clone)
	run(t, clone, "git", "commit", "-q", "--allow-empty", "-m", "base")
	run(t, clone, "git", "push", "-q", "origin", "main")
	run(t, clone, "git", "commit", "-q", "--allow-empty", "-m", "fix")
	sha := run(t, clone, "git", "rev-parse", "HEAD")
	run(t, clone, "git", "remote", "set-url", "origin", "fakehost:"+bare)

	scripts := t.TempDir()
	marker := filepath.Join(scripts, "ssh-called")
	ssh := filepath.Join(scripts, "fakessh")
	if err := os.WriteFile(ssh, []byte(`#!/bin/sh
echo "$*" >> "`+marker+`"
shift
exec sh -c "$(echo "$*" | sed 's/^git-/git /')"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	s6DaemonGitHome(t, "[core]\n\tsshCommand = "+ssh+"\n[ssh]\n\tvariant = simple\n")

	if err := gitPushToOrigin(context.Background(), clone, "fix/issue-9", sha, "tok"); err != nil {
		t.Fatalf("push over ssh: %v", err)
	}
	if got := run(t, bare, "git", "rev-parse", "refs/heads/fix/issue-9"); got != sha {
		t.Fatalf("remote ref %s != pushed %s", got, sha)
	}
	calls, err := os.ReadFile(marker)
	if err != nil || !strings.Contains(string(calls), "fakehost") {
		t.Fatalf("the operator's core.sshCommand did not run the push: %v %q", err, calls)
	}
}

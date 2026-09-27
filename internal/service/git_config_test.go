package service

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/spawn"
)

// Process-spawn law S6-D4, the startup step: the daemon composes its git
// config under the data dir from the operator's HOME, registers it for every
// daemon git command, and hands the doctor what it dropped.
func TestComposeDaemonGitConfig_ComposesUnderTheDataDirAndRegisters(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[user]\n\tname = Op\n[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	data := t.TempDir()
	t.Setenv("VORNIK_DATA_DIR", data)
	restore := spawn.RegisterGitGlobalConfig(spawn.GitGlobalConfig())
	defer restore()

	var logs bytes.Buffer
	c := &Container{Logger: zerolog.New(&logs)}
	c.composeDaemonGitConfig()

	want := filepath.Join(data, "git", "config")
	if spawn.GitGlobalConfig() != want {
		t.Fatalf("registered %q, want %q", spawn.GitGlobalConfig(), want)
	}
	got := c.gitConfigComposition
	if got == nil || got.Err != "" || got.Path != want || got.Examined != 2 || got.Kept != 1 {
		t.Fatalf("composition for the doctor = %+v", got)
	}
	if len(got.Dropped) != 1 || got.Dropped[0].Key != "filter.lfs.clean" || got.Dropped[0].Reason == "" {
		t.Fatalf("dropped = %+v", got.Dropped)
	}
	if !strings.Contains(logs.String(), "filter.lfs.clean") {
		t.Errorf("the startup log must name the dropped key: %s", logs.String())
	}
}

// A composition that cannot be written fails closed: nothing registered, the
// doctor gets the error.
func TestComposeDaemonGitConfig_FailureFailsClosed(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VORNIK_DATA_DIR", blocker) // <file>/git cannot be created
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	restore := spawn.RegisterGitGlobalConfig("")
	defer restore()

	c := &Container{Logger: zerolog.Nop()}
	c.composeDaemonGitConfig()

	if spawn.GitGlobalConfig() != "" {
		t.Fatalf("a failed composition must register nothing, got %q", spawn.GitGlobalConfig())
	}
	if c.gitConfigComposition == nil || c.gitConfigComposition.Err == "" {
		t.Fatalf("the doctor must receive the error, got %+v", c.gitConfigComposition)
	}
}

func TestGitConfigDataDir(t *testing.T) {
	t.Setenv("VORNIK_DATA_DIR", "/srv/vornik")
	if got := gitConfigDataDir(); got != "/srv/vornik" {
		t.Errorf("data dir = %q", got)
	}
	t.Setenv("VORNIK_DATA_DIR", "")
	if got := gitConfigDataDir(); !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("fallback = %q, want under %s", got, os.TempDir())
	}
}

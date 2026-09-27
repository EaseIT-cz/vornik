package github

import (
	"context"
	"errors"
	"os"
	"testing"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/spawn"
)

// The process-spawn law's GitWorkspace kind refuses a directory outside a
// registered workspace root (design "S1b-2, as built"); the daemon registers
// runtime.project_workspace_path. These tests run git in t.TempDir()
// repositories, so the temp root is theirs.
func init() {
	if err := spawn.RegisterWorkspaceRoot(os.TempDir()); err != nil {
		panic(err)
	}
}

// The push runs as the spawn law's GitWorkspace (S1b-2): a clone outside the
// registered workspace root is refused before git starts, and the refusal is
// not a remote rejection.
func TestGitPushToOrigin_RefusesOutsideTheWorkspaceRoot(t *testing.T) {
	err := gitPushToOrigin(context.Background(), "/etc", "b", "abc", "tok")
	if !errors.Is(err, spawn.ErrRefused) {
		t.Fatalf("want spawn.ErrRefused, got %v", err)
	}
	var rej *forge.PushRejectedError
	if errors.As(err, &rej) {
		t.Fatal("a refusal is not a remote push rejection")
	}
}

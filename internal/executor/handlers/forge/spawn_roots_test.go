package forge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/spawn"
)

// The process-spawn law's GitWorkspace and GitHTTPBackend kinds refuse a
// directory outside a registered workspace root (design "S1b-2, as built");
// the daemon registers runtime.project_workspace_path. These tests run git in
// t.TempDir() repositories, so the temp root is theirs.
func init() {
	if err := spawn.RegisterWorkspaceRoot(os.TempDir()); err != nil {
		panic(err)
	}
}

// WorkspaceHead is the service's publish-source read, moved here so the service
// wiring spawns nothing (process-spawn law, S1b-2). It reads HEAD inside the
// registered workspace root and refuses a directory outside it.
func TestWorkspaceHead(t *testing.T) {
	if _, err := execLookGit(); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "commit", "--allow-empty", "-m", "base")
	want := gitOut(t, dir, "rev-parse", "HEAD")
	got, err := WorkspaceHead(context.Background(), dir)
	if err != nil || got != want {
		t.Fatalf("WorkspaceHead = %q, %v; want %q", got, err, want)
	}
	if _, err := WorkspaceHead(context.Background(), "/etc"); !errors.Is(err, spawn.ErrRefused) {
		t.Fatalf("outside the workspace root: want spawn.ErrRefused, got %v", err)
	}
	if _, err := WorkspaceHead(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing directory must error")
	}
}

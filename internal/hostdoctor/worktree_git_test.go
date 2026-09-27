package hostdoctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanWorktreeGit_PrunesAndDeletesTheBranch(t *testing.T) {
	var calls []string
	orig := gitRun
	gitRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { gitRun = orig })

	notes := CleanWorktreeGit(context.Background(), "/ws", []string{"proj/task_x", "bad", "../etc/task"})
	want := []string{
		"-C " + filepath.Join("/ws", "proj") + " worktree prune",
		"-C " + filepath.Join("/ws", "proj") + " branch -D -- worktree/task_x",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("git calls = %q, want %q", calls, want)
	}
	if len(notes) != 2 {
		t.Fatalf("malformed entries must be skipped and reported: %q", notes)
	}
}

// The `removed` list crosses from the daemon to vornikctl, which runs git on
// it, so no entry may make git run outside the workspaces root (S2 code review,
// review-20260926-f198 #3). Each shape here is refused before any git call.
func TestCleanWorktreeGit_NeverRunsOutsideTheWorkspacesRoot(t *testing.T) {
	var calls []string
	orig := gitRun
	gitRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { gitRun = orig })

	hostile := []string{
		"/etc/passwd/foo", // absolute project
		"foo/../bar",      // traversal in the task
		"../x",            // parent project
		"./x",             // current directory
		"a\\b/t",          // backslash separator
		"p/t\x00x",        // NUL in the task
		"p\n/t",           // control character in the project
		" /t",             // blank project
	}
	notes := CleanWorktreeGit(context.Background(), "/ws", hostile)
	if len(calls) != 0 {
		t.Fatalf("git ran for a hostile entry: %q", calls)
	}
	if len(notes) != len(hostile) {
		t.Fatalf("every hostile entry must be reported as skipped, got %q", notes)
	}
}

func TestCleanWorktreeGit_NeedsAWorkspacesRoot(t *testing.T) {
	notes := CleanWorktreeGit(context.Background(), "", []string{"proj/task_x"})
	if len(notes) != 1 || !strings.Contains(notes[0], "skipped") {
		t.Fatalf("notes = %q", notes)
	}
}

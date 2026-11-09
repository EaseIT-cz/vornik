package agentloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #455 (agent tool-dispatch design, amendment 2026-11-07). Found at:
// review (the 2026-10-19 code audit), observed at e2e-lane (a step reading a
// file another process swaps for a symlink). file_read and read_many_files
// resolved and checked a path, then opened it by name, so a swap in between
// read outside the workspace. Regression test at: unit, with the swap made
// deterministic by the afterResolve seam (AGENTS.md §3). Lower because: the
// race needs a concurrent writer the shipped roles do not have.

func swapToOutside(t *testing.T, _, name, secret string) {
	t.Helper()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := afterResolve
	afterResolve = func(resolved string) {
		if filepath.Base(resolved) != name {
			return
		}
		_ = os.Remove(resolved)
		if err := os.Symlink(outside, resolved); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterResolve = prev })
}

func TestFileRead_455_ASwapAfterResolveCannotLeaveTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapToOutside(t, ws, "notes.txt", "OUTSIDE-SECRET")
	out := fileRead(Env{Workspace: ws}, json.RawMessage(`{"path":"notes.txt"}`))
	if strings.Contains(out, "OUTSIDE-SECRET") || !strings.HasPrefix(out, "ERROR") {
		t.Fatalf("file_read followed a swapped symlink out of the workspace: %q", out)
	}
}

func TestReadManyFiles_455_ASwapAfterResolveCannotLeaveTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	for _, f := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(ws, f), []byte("inside "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	swapToOutside(t, ws, "b.txt", "OUTSIDE-SECRET")
	out := readManyFiles(Env{Workspace: ws}, json.RawMessage(`{"paths":["a.txt","b.txt"]}`))
	if strings.Contains(out, "OUTSIDE-SECRET") {
		t.Fatalf("read_many_files followed a swapped symlink out of the workspace: %q", out)
	}
	if !strings.Contains(out, "inside a.txt") {
		t.Fatalf("the other file must still be read: %q", out)
	}
}

// A symlink that stays inside the workspace is still followed.
func TestFileRead_455_AnInsideSymlinkIsRead(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "real.txt"), []byte("real content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if out := fileRead(Env{Workspace: ws}, json.RawMessage(`{"path":"link.txt"}`)); out != "real content" {
		t.Fatalf("an inside symlink must be read: %q", out)
	}
	if out := readManyFiles(Env{Workspace: ws}, json.RawMessage(`{"paths":["link.txt"]}`)); !strings.Contains(out, "real content") {
		t.Fatalf("read_many_files: %q", out)
	}
}

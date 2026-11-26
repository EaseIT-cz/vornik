package agentloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GitHub #843: in the #809 re-calibration at 557d5c1b5 (gemma4-26b), 18 of 196
// runs were killed for "repeated file_write 4 times": a write of the bytes the
// file already held was answered "OK: wrote N bytes", so the model wrote again.
// These tests pin the reply that says nothing was written and to go on.
//
// Found at: e2e-lane (the agent-bench calibration arm). Regression test at:
// unit here, and integration test/agent/tool_dispatch_golden_test.sh
// (file_write-unchanged, through the real entrypoint).

func write843(t *testing.T, ws, path, content string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"path": path, "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return Dispatch(Env{Workspace: ws}, "file_write", raw)
}

func TestFileWrite_SameBytesAreNotWritten_843(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "main.go")
	mustWrite(t, p, "package main\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	out := write843(t, ws, "main.go", "package main\n")
	if !strings.HasPrefix(out, "OK: ") || !strings.Contains(out, "already holds exactly this content (13 bytes); nothing was written") ||
		!strings.Contains(out, "go on with your next step") {
		t.Fatalf("reply = %q", out)
	}
	st, _ := os.Stat(p)
	if !st.ModTime().Equal(old) {
		t.Errorf("the file was rewritten: mtime %v, want %v", st.ModTime(), old)
	}
}

func TestFileWrite_DifferentBytesOfTheSameSizeAreWritten_843(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "main.go")
	mustWrite(t, p, "package main\n")
	out := write843(t, ws, "main.go", "package hexa\n")
	if !strings.HasPrefix(out, "OK: wrote 13 bytes to ") {
		t.Fatalf("reply = %q", out)
	}
	if b, _ := os.ReadFile(p); string(b) != "package hexa\n" {
		t.Errorf("file holds %q", b)
	}
}

func TestFileWrite_ANewFileIsWritten_843(t *testing.T) {
	ws := t.TempDir()
	if out := write843(t, ws, "new.go", "package new\n"); !strings.HasPrefix(out, "OK: wrote 12 bytes to ") {
		t.Fatalf("reply = %q", out)
	}
}

// A read-only file is refused by its mode before any comparison, even when
// the content matches.
func TestFileWrite_ReadOnlyIsStillRefusedFirst_843(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "staged.md")
	mustWrite(t, p, "doc\n")
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	if out := write843(t, ws, "staged.md", "doc\n"); !strings.HasPrefix(out, "ERROR: the file is read-only: ") {
		t.Fatalf("reply = %q", out)
	}
}

// A path through a symlink is resolved to its target first (resolvePath), so
// the comparison is against the target's bytes, and the reply names the
// target (review cf85 F2).
func TestFileWrite_ASymlinkIsComparedAsItsTarget_843(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "target.txt"), "same\n")
	if err := os.Symlink("target.txt", filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	out := write843(t, ws, "link.txt", "same\n")
	if !strings.Contains(out, "target.txt already holds exactly this content") {
		t.Fatalf("reply = %q", out)
	}
}

// A directory at the path keeps its old error: it is never compared.
func TestFileWrite_ADirectoryKeepsItsError_843(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := write843(t, ws, "dir", "x")
	if !strings.HasPrefix(out, "ERROR: ") || strings.Contains(out, "already holds") {
		t.Fatalf("reply = %q", out)
	}
}

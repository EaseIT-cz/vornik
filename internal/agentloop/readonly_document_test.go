package agentloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Broker design §18.7 F6 (a document input, review 7514): a staged document
// is mode 0444 so the role cannot change it in place. file_write is refused
// by the open; file_edit replaced the file by rename, which a read-only mode
// does not stop, so it now refuses a file without a write bit.
func TestFileTools_ReadOnlyDocumentIsNotChanged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	ws := t.TempDir()
	doc := filepath.Join(ws, "artifacts", "in", "design.md")
	mustWrite(t, doc, "original text")
	if err := os.Chmod(doc, 0o444); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tool, args string }{
		{"file_edit", `{"path":"artifacts/in/design.md","old_string":"original","new_string":"forged"}`},
		{"file_write", `{"path":"artifacts/in/design.md","content":"forged"}`},
	} {
		got := Dispatch(Env{Workspace: ws}, c.tool, json.RawMessage(c.args))
		if !strings.HasPrefix(got, "ERROR:") {
			t.Errorf("%s changed a read-only document: %s", c.tool, got)
		}
		data, _ := os.ReadFile(doc)
		if string(data) != "original text" {
			t.Fatalf("%s rewrote the document: %q", c.tool, data)
		}
	}
	// Review 20261003-6fec item 1: file_write refuses by the file's mode
	// itself, not only by the OS refusing the open, so the guarantee does not
	// rest on the container's user lacking privilege.
	if got := Dispatch(Env{Workspace: ws}, "file_write", json.RawMessage(`{"path":"artifacts/in/design.md","content":"forged"}`)); !strings.Contains(got, "read-only") {
		t.Errorf("file_write did not name the read-only file: %s", got)
	}
	// A writable file is still edited.
	other := filepath.Join(ws, "notes.md")
	mustWrite(t, other, "original text")
	if got := Dispatch(Env{Workspace: ws}, "file_edit", json.RawMessage(`{"path":"notes.md","old_string":"original","new_string":"edited"}`)); !strings.HasPrefix(got, "OK") {
		t.Fatalf("a writable file was refused: %s", got)
	}
}

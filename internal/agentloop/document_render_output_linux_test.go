//go:build linux

package agentloop

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDocumentRenderNamedPipeRefusesWithoutWaitingForWriter(t *testing.T) {
	ws := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(ws, "pipe.md"), 0600); err != nil {
		t.Fatal(err)
	}
	got := documentRender(Env{Workspace: ws}, []byte(`{"path":"pipe.md","format":"html"}`))
	if !strings.Contains(got, `"stage":"read"`) {
		t.Fatalf("FIFO accepted: %s", got)
	}
}

func TestDocumentRenderNestedOutput(t *testing.T) {
	ws := t.TempDir()
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := writeRenderedDocument(root, "artifacts/out/sub/report.html", []byte("rendered")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "artifacts/out/sub/report.html"))
	if err != nil || string(data) != "rendered" {
		t.Fatalf("nested output: %q %v", data, err)
	}
}

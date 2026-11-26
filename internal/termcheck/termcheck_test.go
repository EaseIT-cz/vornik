package termcheck

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// GitHub #865: /dev/null is a character device but not a terminal.
func TestIsTerminal_NotTerminals(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	file, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	for name, f := range map[string]*os.File{"/dev/null": devnull, "file": file, "pipe": r, "nil": nil} {
		if IsTerminal(f) {
			t.Errorf("IsTerminal(%s) = true", name)
		}
	}
}

// A pseudo-terminal is one, so the check is not simply always false.
func TestIsTerminal_PtyIsATerminal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the pty master answers the terminal ioctl on Linux")
	}
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx here: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	if !IsTerminal(ptmx) {
		t.Error("IsTerminal(pty) = false")
	}
}

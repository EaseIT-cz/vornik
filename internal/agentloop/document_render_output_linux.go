//go:build linux

package agentloop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Nonblocking open makes special files refuse immediately rather than hanging
// before the regular-file check (the converter deadline starts afterwards).
func openDocumentSource(root *os.Root, src string) (*os.File, error) {
	return root.OpenFile(src, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// Refuse all output directory aliases, including within-workspace symlinks.
// Holding each directory descriptor avoids a check-then-open symlink race.
func writeRenderedDocument(root *os.Root, dst string, content []byte) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	// documentPaths already restricts dst to artifacts/out/<...>.
	for _, component := range strings.Split(filepath.Dir(dst), string(os.PathSeparator)) {
		if err := syscall.Mkdirat(int(dir.Fd()), component, 0755); err != nil && !errors.Is(err, syscall.EEXIST) {
			return err
		}
		fd, err := syscall.Openat(int(dir.Fd()), component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		_ = dir.Close()
		dir = os.NewFile(uintptr(fd), component)
	}
	name := filepath.Base(dst)
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), dst)
	_, writeErr := f.Write(content)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = syscall.Unlinkat(int(dir.Fd()), name)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	return nil
}

//go:build linux

package agentloop

import (
	"errors"
	"os"
	"os/exec"
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

// isolateDocumentChild starts a renderer child in a fresh user and network
// namespace: only a loopback interface, so pandoc and WeasyPrint are
// network-off whatever the role's network mode (#339 §0.3). The helper's own
// uid/gid map 1:1, so file ownership is unchanged. Where the kernel or seccomp
// refuses an unprivileged user namespace the child fails to start and the
// call reports it; there is no un-isolated fallback.
func isolateDocumentChild(cmd *exec.Cmd) error {
	uid, gid := os.Getuid(), os.Getgid()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	return nil
}

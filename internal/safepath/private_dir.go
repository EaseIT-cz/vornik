// Package safepath resolves paths that must stay inside a root, and makes
// directories under the shared temp dir that only the daemon controls.
package safepath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnsurePrivateDir makes path a directory only the current user controls,
// when path is under the shared temp dir (support-report design, addendum
// 2026-11-05, GitHub #461). There another local user can create the name
// first, and os.MkdirAll would accept their directory or follow their
// symlink. Outside the temp dir (a configured data or artifacts directory)
// it only creates the directory, as before: such a path cannot be planted by
// an ordinary local user, and its owner may legitimately be another account
// (a container's bind mount).
//
// Under the temp dir every component below it is checked in turn, the path's
// parents first: a missing one is created with mode 0700 (os.Mkdir, so a name
// planted between check and create fails instead of being followed); then,
// read with Lstat, it must be a real directory (not a symlink) and, on Unix,
// owned by the current user with no group or other write bit. Otherwise it
// returns an error naming the component, and the caller must not write there.
func EnsurePrivateDir(path string) error {
	if !underTempDir(path) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create dir %s: %w", path, err)
		}
		return nil
	}
	tmp, _ := filepath.Abs(os.TempDir())
	abs, _ := filepath.Abs(path)
	rel, _ := filepath.Rel(tmp, abs)
	dir := tmp
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		if err := ensurePrivateComponent(dir); err != nil {
			return err
		}
	}
	return nil
}

// ensurePrivateComponent creates dir with mode 0700 when it is missing and
// checks that it is a directory only the current user controls.
func ensurePrivateComponent(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create private dir %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("private dir %s: %w", dir, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("private dir %s under the temp dir is a symlink; the operator must remove it", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("private dir %s under the temp dir is not a directory; the operator must remove it", dir)
	}
	if reason := notPrivate(fi); reason != "" {
		return fmt.Errorf("private dir %s under the temp dir %s; the operator must remove it", dir, reason)
	}
	return nil
}

// underTempDir reports whether path is inside os.TempDir(). The test is
// lexical: every caller builds its path from os.TempDir() itself, so the two
// share one form. An Abs error (unreachable in practice) reads as outside.
func underTempDir(path string) bool {
	tmp, err := filepath.Abs(os.TempDir())
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(tmp, abs)
	// The temp dir itself is shared by design (mode 1777), not a private
	// root: only paths strictly inside it are checked.
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

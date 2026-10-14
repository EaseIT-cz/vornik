// Package repowalk holds the one rule tests use for which directories a walk
// of the repository skips.
package repowalk

import (
	"path/filepath"
	"strings"
)

// SkipDir reports whether a walk rooted at root skips the directory at path,
// whose base name is name. The root is never skipped, whatever it is named:
// publish-ce runs the exported tree's tests inside $SRC/.vornik-export, and a
// walk that skipped dot-directories including its own root examined nothing
// there (2026.9.0 imagemanifest; 2026.10.6, four laws). Below the root, hidden
// directories (.git, .claude worktrees, the CE export copies), vendor and
// node_modules are skipped.
func SkipDir(root, path, name string) bool {
	if filepath.Clean(path) == filepath.Clean(root) {
		return false
	}
	return strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules"
}

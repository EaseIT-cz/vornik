package agentbench

// GitHub #790 (agent benchmark design, amendment #790): a release-gate run
// refuses a published task set. The yearly set lives in tasksets/unreleased/,
// which the Community export removes; every other set is published.

import (
	"path/filepath"
	"strings"
)

// IsUnreleasedTaskSet reports whether path, cleaned and made absolute, is
// inside a directory named "unreleased" whose parent is named "tasksets".
// It guards against a mistake, not against a published set copied there.
func IsUnreleasedTaskSet(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for dir := filepath.Dir(abs); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "unreleased" && filepath.Base(filepath.Dir(dir)) == "tasksets" {
			return true
		}
	}
	return false
}

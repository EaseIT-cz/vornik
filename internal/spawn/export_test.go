package spawn

import "testing"

// resetRootsForTest clears the registered workspace roots for one test and
// restores them after it.
func resetRootsForTest(t *testing.T) {
	t.Helper()
	rootsMu.Lock()
	saved := roots
	roots = nil
	rootsMu.Unlock()
	t.Cleanup(func() {
		rootsMu.Lock()
		roots = saved
		rootsMu.Unlock()
	})
}

// SetGitGlobalConfigForTest registers path as the composed global config for
// one test and returns the restore func.
func SetGitGlobalConfigForTest(path string) func() { return RegisterGitGlobalConfig(path) }

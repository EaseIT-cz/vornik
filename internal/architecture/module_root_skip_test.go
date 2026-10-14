package architecture

import "testing"

// audit 2026-10-13: the test-env law scanned .vornik-export/ and
// .vornik-public-clone/ (untracked CE export copies of OLDER code) and failed
// the gate stamp on 7f8307437. Every hidden directory below the root is skipped.
func TestSkipNonModuleDir(t *testing.T) {
	root := "/r"
	cases := []struct {
		path, name string
		skip       bool
	}{
		{"/r", "r", false},
		{"/r/.vornik-export", ".vornik-export", true},
		{"/r/.vornik-public-clone", ".vornik-public-clone", true},
		{"/r/.claude", ".claude", true},
		{"/r/.git", ".git", true},
		{"/r/internal/x/.hidden", ".hidden", true},
		{"/r/vendor", "vendor", true},
		{"/r/web/node_modules", "node_modules", true},
		{"/r/internal", "internal", false},
		{"/r/test/e2e", "e2e", false},
	}
	for _, c := range cases {
		if got := skipNonModuleDir(root, c.path, c.name); got != c.skip {
			t.Errorf("skipNonModuleDir(%q) = %v, want %v", c.path, got, c.skip)
		}
	}
}

package agentns

import "testing"

// GitHub #920 (agent-administered design part 24 §24.6): only a project
// that opted in shares its namespace's home memory.
func TestMemoryProject_920(t *testing.T) {
	for _, tc := range []struct {
		project string
		shared  bool
		want    string
	}{
		{"hermes--prague-venues", true, "hermes--home"},
		{"hermes--prague-venues", false, "hermes--prague-venues"},
		{"hermes--home", true, "hermes--home"},
		{"assistant", true, "assistant"},
		{"dev-project", false, "dev-project"},
		{"", true, ""},
	} {
		if got := MemoryProject(tc.project, tc.shared); got != tc.want {
			t.Errorf("MemoryProject(%q, %v) = %q, want %q", tc.project, tc.shared, got, tc.want)
		}
	}
}

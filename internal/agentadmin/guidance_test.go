package agentadmin

import (
	"os"
	"strings"
	"testing"
)

// Plan P6.3: the guidance carries the rules an unskilled user's safety
// leans on, and the bundle skills carry exactly the same text after their
// frontmatter, so a bundle cannot drift from what the daemon serves.
// Control: admin_guidance.md and the two SKILL.md files.
func TestAdminGuidance(t *testing.T) {
	g := AdminGuidance()
	for _, want := range []string{"describe_installation", "request_credential", "Never ask the user for a password",
		"on their phone", "Writes are proposals", "result", "Stay inside your namespace", "untrusted_content", "schedule", "recent_runs",
		// From the real-model arm (2026-10-02): the two-step connection, said plainly.
		"two steps", "never both", "its own project"} {
		if !strings.Contains(g, want) {
			t.Errorf("the guidance does not say %q", want)
		}
	}
	for _, p := range []string{"../../contrib/claude-code-companion/skills/vornik-admin/SKILL.md", "../../contrib/codex-companion/skills/vornik-admin/SKILL.md", "../../contrib/hermes-companion/skills/vornik-admin/SKILL.md"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(string(raw), "---\n", 3)
		if len(parts) != 3 || !strings.Contains(parts[1], "name: vornik-admin") {
			t.Fatalf("%s: no vornik-admin frontmatter", p)
		}
		if strings.TrimSpace(parts[2]) != g {
			t.Errorf("%s differs from internal/agentadmin/admin_guidance.md; copy it over", p)
		}
	}
}

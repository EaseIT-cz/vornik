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
		"two steps", "never both",
		// Design §18.6 (operator, 2026-10-02): projects are domains, workflows
		// capabilities, swarms teams; tools per role.
		"a project is a domain", "a workflow is a capability", "the swarm is a team", "only the tools its job needs",
		// Design §18.10: how work moves between steps.
		HandoffRule,
		// Design §18.14 finding 2 (round 2 wording, GREEN at review be5a):
		// a client that holds a tool list from before a change has a way out.
		ReconnectRule,
		// Design §19.2: a recipe first.
		RecipeRule, "list_recipes", "install_recipe",
		// Design §18.6 item 2: a role's optional model from the catalogue,
		// and what a remote one costs the user (an approval on the phone).
		"a `model` from the catalogue", "remote", "once per destination"} {
		if !strings.Contains(g, want) {
			t.Errorf("the guidance does not say %q", want)
		}
	}
	// "One project per automation" was the defect §18.6 removed.
	if strings.Contains(g, "its own project") {
		t.Error("the guidance still says to give each automation its own project")
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

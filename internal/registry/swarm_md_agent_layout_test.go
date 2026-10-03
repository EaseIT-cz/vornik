package registry

import "testing"

// Agent-administered design §18.11 (2026-10-03): the agent swarm template
// wrote each role's instructions as a level-2 `## <role>` section, a layout
// this parser never read, so every agent-defined role ran with the built-in
// prelude alone. Files already written in that layout must load their text;
// operator swarms keep their format exactly.

const legacyAgentSwarmBody = `
# docreview roles

Rendered by the agent admin verbs for namespace claudecode.

## reviewer

You review a design.
Quote what you object to.

## Checklist

### Before you start
Read the whole design.

## worker

Do the step you are given.
`

func legacySwarm(id string) []byte {
	return []byte(`---
swarmId: "` + id + `"
leadRole: "reviewer"
roles:
    - name: "reviewer"
      description: "Reviews a design"
      permissions:
        allowedTools: ["file_read"]
    - name: "worker"
      description: "Does one step"
      permissions:
        allowedTools: ["file_read"]
---
` + legacyAgentSwarmBody)
}

func rolePrompt(t *testing.T, sw *Swarm, name string) string {
	t.Helper()
	for _, r := range sw.Roles {
		if r.Name == name {
			return r.SystemPrompt
		}
	}
	t.Fatalf("no role %q", name)
	return ""
}

func TestParseSwarmMarkdown_AgentNamespaceLegacyLayoutLoadsRoleText(t *testing.T) {
	sw, err := ParseSwarmMarkdown(legacySwarm("claudecode--docreview"), "claudecode--docreview.md")
	if err != nil {
		t.Fatal(err)
	}
	// Headings inside the instructions are text, not boundaries (review 9ad7
	// F1): only a heading that names a declared role ends a role's section.
	want := "You review a design.\nQuote what you object to.\n\n## Checklist\n\n### Before you start\nRead the whole design."
	if got := rolePrompt(t, sw, "reviewer"); got != want {
		t.Errorf("reviewer prompt = %q, want %q", got, want)
	}
	if got, want := rolePrompt(t, sw, "worker"), "Do the step you are given."; got != want {
		t.Errorf("worker prompt = %q, want %q", got, want)
	}
}

func TestParseSwarmMarkdown_OperatorSwarmLegacyHeadingsStayIgnored(t *testing.T) {
	sw, err := ParseSwarmMarkdown(legacySwarm("docreview"), "docreview.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range sw.Roles {
		if r.SystemPrompt != "" {
			t.Errorf("operator swarm role %q took %q from a level-2 heading; the operator format must not change", r.Name, r.SystemPrompt)
		}
	}
}

// When the file has a Role prompts section, that is the only source, in an
// agent namespace too.
func TestParseSwarmMarkdown_AgentNamespaceRolePromptsSectionWins(t *testing.T) {
	content := string(legacySwarm("claudecode--docreview")) + "\n## Role prompts\n\n### worker\n\nFrom the section.\n"
	sw, err := ParseSwarmMarkdown([]byte(content), "claudecode--docreview.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := rolePrompt(t, sw, "worker"); got != "From the section." {
		t.Errorf("worker prompt = %q, want the Role prompts text", got)
	}
	if got := rolePrompt(t, sw, "reviewer"); got != "" {
		t.Errorf("reviewer prompt = %q: the legacy layout must not apply beside a Role prompts section", got)
	}
}

// A duplicate role heading: the first section wins, and the later one stays
// text of the role before it.
func TestParseSwarmMarkdown_AgentNamespaceDuplicateRoleHeadingFirstWins(t *testing.T) {
	content := string(legacySwarm("claudecode--docreview")) + "\n## reviewer\n\nA second reviewer section.\n"
	sw, err := ParseSwarmMarkdown([]byte(content), "claudecode--docreview.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := rolePrompt(t, sw, "reviewer"); got == "" || got[:20] != "You review a design." {
		t.Errorf("reviewer prompt = %q, want the first section", got)
	}
	if got, want := rolePrompt(t, sw, "worker"), "Do the step you are given.\n\n## reviewer\n\nA second reviewer section."; got != want {
		t.Errorf("worker prompt = %q, want %q", got, want)
	}
}

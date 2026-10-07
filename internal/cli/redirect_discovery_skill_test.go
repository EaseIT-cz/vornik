package cli

import (
	"os"
	"path/filepath"
	"testing"
	"vornik.io/vornik/internal/registry"
)

// Issue #56: the shipped capability must be importable into a real configs tree.
func TestRedirectDiscoverySkill_Import(t *testing.T) {
	dir := buildTestConfigsDir(t)
	path := filepath.Join("..", "..", "contrib", "skills", "redirect-discovery.swarm-skill.md")
	out, err := runSkillImportForTest(t, path, func() { skillImportConfigsDir = dir; skillImportIntoSwarm = "my-swarm" })
	if err != nil {
		t.Fatalf("import: %v; %s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workflows", "redirect-discovery.md"))
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := registry.ParseWorkflowMarkdown(raw, "redirect-discovery.md")
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Entrypoint != "investigate" || workflow.Steps["investigate"].Role != "redirect-researcher" || workflow.Terminals["done"].Status != "COMPLETED" {
		t.Fatalf("unrunnable imported workflow: %+v", workflow)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "swarms", "my-swarm.md"))
	if err != nil {
		t.Fatal(err)
	}
	swarm, err := registry.ParseSwarmMarkdown(raw, "my-swarm.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(swarm.Roles) != 2 || swarm.Roles[0].Name != "lead" {
		t.Fatal("import lost existing role")
	}
	role := swarm.Roles[1]
	if role.Name != "redirect-researcher" {
		t.Fatal("missing discovery role")
	}
	hasFetch := false
	for _, tool := range role.Permissions.AllowedTools {
		if tool == "mcp__scraper__web_fetch" {
			hasFetch = true
		}
		if tool == "run_shell" || tool == "api_call" {
			t.Fatalf("unexpected write/network capability: %s", tool)
		}
	}
	if !hasFetch || len(role.RequiredOutputKeys) == 0 || role.SystemPrompt == "" {
		t.Fatal("imported role lacks executable procedure and output contract")
	}
}

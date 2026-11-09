package harnessconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

// GitHub #589 (25-claude-desktop-companion.md §B.3): a companion entry sits
// beside agent entries in Claude Desktop's config, named
// vornik-companion-<name>, running `companion mcp-bridge --name <name>`, and
// set or removed without touching the agent entry.
func TestCompanionEntry_589_SetAndRemoveBesideAnAgentEntry(t *testing.T) {
	agent := Entry{Namespace: "hermes", Command: "/usr/local/bin/vornikctl"}
	comp := Entry{Kind: KindCompanion, Namespace: "default", Command: "/usr/local/bin/vornikctl", URL: "https://vornik.example"}
	if comp.Name() != "vornik-companion-default" || agent.Name() != "vornik-hermes" {
		t.Fatalf("names %q %q", comp.Name(), agent.Name())
	}
	if got := strings.Join(comp.Args(), " "); got != "companion mcp-bridge --name default --url https://vornik.example" {
		t.Fatalf("companion args %q", got)
	}
	content := []byte(`{"mcpServers":{"other":{"command":"x"}}}`)
	content, err := Set(ClaudeDesktop, content, agent)
	if err != nil {
		t.Fatal(err)
	}
	content, err = Set(ClaudeDesktop, content, comp)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(content, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.MCPServers) != 3 || m.MCPServers["vornik-companion-default"].Args[0] != "companion" {
		t.Fatalf("servers %v", m.MCPServers)
	}
	content, found, err := Remove(ClaudeDesktop, content, comp)
	if err != nil || !found {
		t.Fatalf("remove: %v %v", found, err)
	}
	if !strings.Contains(string(content), `"vornik-hermes"`) || strings.Contains(string(content), "vornik-companion-default") {
		t.Fatalf("removing the companion entry must leave the agent entry: %s", content)
	}
	if _, _, err := Remove(ClaudeDesktop, content, Entry{Kind: KindCompanion, Namespace: "Bad Name"}); err == nil {
		t.Fatal("an invalid name is refused")
	}
}

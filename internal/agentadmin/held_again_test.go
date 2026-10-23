package agentadmin

import (
	"strings"
	"testing"
)

// GitHub #481 (2026-10-21): suspension not sticky; no re-list between
// reloads. A re-add is the person's re-approval of tools Vornik held as
// changing things, so its sentence says which (adversarial code review F2:
// after a remove the history was invisible and the re-add read as a
// routine approval). Agent-administered design §6.5, amendment #481 item 5.

func TestAddMCPServer481_ReaddNamesTheHeldTools(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	tr.approveServer("finance", "fio", "https://api.fio.example/mcp", "balance", "statement")
	tr.apply(tr.render(VerbRemove, RemoveInput{Kind: "integration", Project: "finance", ID: "fio"}))
	a := tr.approvals["hermes--finance"]["fio"]
	a.Suspended = []string{"statement"}
	tr.approvals["hermes--finance"]["fio"] = a
	tr.advert["https://api.fio.example/mcp"] = []string{"balance", "statement"}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "finance", Name: "fio", URL: "https://api.fio.example/mcp"})
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "Vornik had stopped statement on that server") || strings.Contains(c.Sentence, "stopped balance") {
		t.Fatalf("sentence %q", c.Sentence)
	}
	// Nothing held: no clause.
	a.Suspended = nil
	tr.approvals["hermes--finance"]["fio"] = a
	if c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "finance", Name: "fio", URL: "https://api.fio.example/mcp"}); strings.Contains(c.Sentence, "Vornik had stopped") {
		t.Fatalf("a clause with nothing held: %q", c.Sentence)
	}
}

func TestInstallRecipe481_NewEntryNamesTheHeldTools(t *testing.T) {
	tr := probeTree(t)
	tr.approvals["hermes--personal"] = map[string]IntegrationApproval{
		"src": {Kind: "mcp", URL: probeURL, Read: []string{"look", "peek"}, Removed: true, Suspended: []string{"peek", "scan"}},
	}
	c := tr.render(VerbInstallRecipe, probeInstall(nil))
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Sentence, "Vornik had stopped peek on that server") || strings.Contains(c.Sentence, "scan on that server") {
		t.Fatalf("sentence %q", c.Sentence)
	}
}

package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

// GitHub #405 (2026-10-16): a mutating tool was approved as a read. These
// are the renderer's half of design §6.4: the read set, the withheld clause
// in the sentence and the plain summary, the fail-closed default, and the
// recipe paths. The service tests drive the classifier and the gate.

const mailURL = "https://mail.example/mcp"

func commsTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "comms", Purpose: "Mail"}))
	return tr
}

func TestAddMCPServer_WithholdsWhatTheClassifierCallsMutating(t *testing.T) {
	tr := commsTree(t)
	tr.advert[mailURL] = []string{"search_messages", "gmail_create_draft"}
	tr.reads = map[string][]string{mailURL: {"search_messages"}}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: mailURL, Auth: MCPAuthInput{Mode: "none"}})
	tr.mustClass(c, Widening)
	g := c.Grant.Integrations[0]
	if strings.Join(g.Read, ",") != "search_messages" || len(g.Write) != 0 || g.ReadPending {
		t.Fatalf("grant %+v", g)
	}
	if strings.Join(c.Withheld, ",") != "gmail_create_draft" {
		t.Fatalf("withheld %v", c.Withheld)
	}
	if !strings.Contains(c.Sentence, "these 1 read tools: search_messages. It also offers 1 tools that change things, which are not included: gmail_create_draft.") {
		t.Fatalf("sentence: %s", c.Sentence)
	}
	if c.Plain == nil || !strings.Contains(c.Plain.Summary, "tools that change things; those are not included") {
		t.Fatalf("plain: %+v", c.Plain)
	}
	tr.apply(c)
	if got := tr.state().Projects["hermes--comms"].Servers[0].Tools; strings.Join(got, ",") != "search_messages" {
		t.Fatalf("rendered allowed tools %v", got)
	}

	// Control: nothing withheld leaves the sentence and summary as before.
	tr2 := commsTree(t)
	tr2.advert[mailURL] = []string{"search_messages"}
	c = tr2.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: mailURL, Auth: MCPAuthInput{Mode: "none"}})
	if strings.Contains(c.Sentence, "change things") || strings.Contains(c.Plain.Summary, "change things") || c.Withheld != nil {
		t.Fatalf("a clean server is said to change things: %s / %s", c.Sentence, c.Plain.Summary)
	}
}

// Fail closed: a listing that carries no verdict withholds every tool, and
// with nothing left and no write_tools the add is refused, naming them.
func TestAddMCPServer_NoVerdictWithholdsEverything(t *testing.T) {
	tr := commsTree(t)
	tr.advert[mailURL] = []string{"search_messages", "gmail_create_draft"}
	tr.reads = map[string][]string{}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "mail", URL: mailURL, Auth: MCPAuthInput{Mode: "none"}})
	tr.mustClass(c, Refused)
	for _, want := range []string{"only tools that change things", "gmail_create_draft, search_messages", "write_tools"} {
		if !strings.Contains(c.Reason, want) {
			t.Errorf("reason lacks %q: %s", want, c.Reason)
		}
	}
}

// The vornik-gws-mcp write endpoint shape: no reads, one declared write.
func TestAddMCPServer_EmptyReadSetWithWriteToolsIsAllowed(t *testing.T) {
	tr := commsTree(t)
	tr.advert[mailURL] = []string{"gmail_create_draft"}
	tr.reads = map[string][]string{}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "comms", Name: "drafts", URL: mailURL,
		Auth: MCPAuthInput{Mode: "none"}, WriteTools: []string{"gmail_create_draft"}})
	tr.mustClass(c, Widening)
	g := c.Grant.Integrations[0]
	if len(g.Read) != 0 || strings.Join(g.Write, ",") != "gmail_create_draft" || len(c.Withheld) != 0 {
		t.Fatalf("grant %+v withheld %v", g, c.Withheld)
	}
}

func TestApproveServerTools_WithholdsWhatTheClassifierCallsMutating(t *testing.T) {
	tr := treeWithPendingServer(t)
	c := tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--fin", Server: "bank",
		Tools: []string{"get_balance", "transfer_money"}, Reads: []string{"get_balance"}})
	tr.mustClass(c, Widening)
	if got := strings.Join(c.Grant.Integrations[0].Read, ","); got != "get_balance" {
		t.Fatalf("read %s", got)
	}
	if strings.Join(c.Withheld, ",") != "transfer_money" || !strings.Contains(c.Sentence, "which are not included: transfer_money") {
		t.Fatalf("withheld %v sentence %s", c.Withheld, c.Sentence)
	}
	if !strings.Contains(c.Plain.Summary, "tools that change things") {
		t.Fatalf("plain: %s", c.Plain.Summary)
	}

	// Fail closed: no verdict (the tree helper fills one, so render
	// directly) leaves nothing and files nothing, naming the tools in the
	// typed field the service builds its note from.
	raw, _ := json.Marshal(ApproveServerToolsInput{Project: "hermes--fin", Server: "bank", Tools: []string{"get_balance", "transfer_money"}})
	c, err := tr.r.Render(tr.state(), VerbApproveServerTools, raw)
	if err != nil {
		t.Fatal(err)
	}
	tr.mustClass(c, Refused)
	if strings.Join(c.Withheld, ",") != "get_balance,transfer_money" {
		t.Fatalf("withheld %v", c.Withheld)
	}
}

// A recipe never approves a write as a read: at install, when the server
// was listed, the descriptor decides (a read-verb name the server marks
// destructive is refused); at the tools approval, likewise.
func TestInstallRecipe_RefusesADeclaredReadTheListingCallsMutating(t *testing.T) {
	tr := probeTree(t)
	// "peek" is a read by name nowhere, "look" neither; what matters is the
	// listing's verdict: look is a read, peek was judged mutating (say,
	// destructiveHint: true).
	tr.reads = map[string][]string{probeURL: {"look", "other", "scan"}}
	c := tr.render(VerbInstallRecipe, probeInstall(nil))
	tr.mustClass(c, Refused)
	if !strings.Contains(c.Reason, "offers peek as a tool that changes things; this recipe only reads") {
		t.Fatalf("reason: %s", c.Reason)
	}
	// Control: the same listing judging both reads installs.
	tr.reads = map[string][]string{probeURL: {"look", "peek"}}
	tr.mustClass(tr.render(VerbInstallRecipe, probeInstall(nil)), Widening)
}

func TestApproveServerTools_RecipeDeclaredMutatingIsRefused(t *testing.T) {
	tr := shippedTree(t)
	tr.apply(tr.render(VerbInstallRecipe, shippedVars("inbox-digest")))
	c := tr.render(VerbApproveServerTools, ApproveServerToolsInput{Project: "hermes--personal", Server: "mail",
		Tools: []string{"gmail_get", "gmail_search", "gmail_send"}, Reads: []string{"gmail_search"}})
	tr.mustClass(c, Refused)
	if strings.Join(c.Withheld, ",") != "gmail_get" || !strings.Contains(c.Reason, "gmail_get as tools that change things") {
		t.Fatalf("withheld %v reason %s", c.Withheld, c.Reason)
	}
}

// Review 923e: a recipe upgrade reusing a listed server refuses an added
// read the listing calls mutating, as a new install does.
func TestInstallRecipe_UpgradeRefusesAnAddedReadTheListingCallsMutating(t *testing.T) {
	tr := probeTree(t)
	tr.apply(tr.render(VerbInstallRecipe, probeInstall(nil)))
	v2 := strings.Replace(probeRecipe, "version: 1\nenvelope", "version: 2\nenvelope", 1)
	v2 = strings.Replace(v2, "read_tools: [look, peek]", "read_tools: [look, peek, scan]", 1)
	v2 = strings.Replace(v2, "tools: [file_write, mcp__src__look, mcp__src__peek]", "tools: [file_write, mcp__src__look, mcp__src__peek, mcp__src__scan]", 1)
	tr.r = rendererWith(t, "", map[string]string{"probe": v2})
	tr.reads = map[string][]string{probeURL: {"look", "other", "peek"}}
	c := tr.render(VerbInstallRecipe, probeInstall(nil))
	tr.mustClass(c, Refused)
	if !strings.Contains(c.Reason, "offers scan as a tool that changes things") {
		t.Fatalf("reason: %s", c.Reason)
	}
	// Control: the listing calls scan a read.
	tr.reads = map[string][]string{probeURL: {"look", "other", "peek", "scan"}}
	tr.mustClass(tr.render(VerbInstallRecipe, probeInstall(nil)), Widening)
}

// The connector install judges recipe-declared tools by name alone (design
// §6.4 item 2), so every shipped recipe's read tool must be a read by name.
func TestShippedRecipes_ReadToolsAreReadsByName(t *testing.T) {
	tr := shippedTree(t)
	views := tr.r.ListRecipes(tr.state())
	if len(views) == 0 {
		t.Fatal("no shipped recipes listed")
	}
	examined := 0
	for _, v := range views {
		for _, n := range v.Needs {
			for _, tool := range n.ReadTools {
				examined++
				if len(readsByNameForTest([]string{tool})) != 1 {
					t.Errorf("recipe %s need %s: %s is not a read by name", v.Name, n.Name, tool)
				}
			}
		}
	}
	if examined == 0 {
		t.Fatal("examined no read tools")
	}
}

package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

// Design §6.1 (2026-10-08): add_mcp_server declares its transport.
// reference host 2026-10-08: hermes--prague-venue-research scraper 404 — the
// agent path hard-coded streamable-http, so an SSE-only server never
// connected. Control: the transport check in addMCPServer, the template's
// transport line, ServerState.Transport and its load in from_registry.go.

const scraperURL = "http://127.0.0.1:8787"

func TestAddMCPServer_TransportAcceptedAndRefused(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("venues")
	for _, ok := range []string{"", "streamable-http", "sse"} {
		if c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "venues", Name: "scraper", URL: scraperURL, Transport: ok}); c.Class != Widening {
			t.Errorf("transport %q refused: %s", ok, c.Reason)
		}
	}
	for _, bad := range []string{"stdio", "http", "SSE", "websocket"} {
		c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "venues", Name: "scraper", URL: scraperURL, Transport: bad})
		if c.Class != Refused {
			t.Errorf("transport %q accepted", bad)
			continue
		}
		if !strings.Contains(c.Reason, `"streamable-http"`) || !strings.Contains(c.Reason, `"sse"`) {
			t.Errorf("refusal of %q does not name the allowed values: %s", bad, c.Reason)
		}
	}
}

func TestAddMCPServer_SSERendersAndSurvivesLaterVerbs(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("venues")
	tr.advert[scraperURL] = []string{"fetch", "post"}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "venues", Name: "scraper", URL: scraperURL, Transport: "sse", WriteTools: []string{"post"}})
	tr.mustClass(c, Widening)
	content := c.Ops[0].Content
	if strings.Count(content, `transport: "sse"`) != 2 || strings.Contains(content, "streamable-http") {
		t.Fatalf("the read entry and its write sibling must both be sse:\n%s", content)
	}
	if g := c.Grant.Integrations[0]; g.Transport != "sse" {
		t.Fatalf("grant transport = %q", g.Transport)
	}
	if !strings.Contains(c.Sentence, "over SSE") {
		t.Fatalf("the sentence does not name the transport: %s", c.Sentence)
	}
	if c.Plain == nil || !strings.Contains(c.Plain.Summary, "SSE") {
		t.Fatalf("the plain summary does not name SSE: %+v", c.Plain)
	}
	tr.apply(c)
	// A later verb re-renders the project from loaded state: sse must stay.
	// tr.state() parses every applied file with the registry's own types and
	// rebuilds it through ProjectStateFrom (tree_test.go), so this is the
	// real reload path (review 5ed3 A8).
	b := tr.render(VerbSetBudget, SetBudgetInput{Project: "venues", MonthlyUSD: 1})
	if len(b.Ops) != 1 || strings.Count(b.Ops[0].Content, `transport: "sse"`) != 2 {
		t.Fatalf("set_budget flipped the transport back:\n%s", b.Ops[0].Content)
	}
	// Review 5ed3 A3: update_project and remove re-render it too.
	u := tr.render(VerbUpdateProject, UpdateProjectInput{Project: "venues", Purpose: json.RawMessage(`"Venue research, Prague"`)})
	if u.Class == Refused || len(u.Ops) != 1 || strings.Count(u.Ops[0].Content, `transport: "sse"`) != 2 {
		t.Fatalf("update_project flipped the transport back: %s\n%+v", u.Reason, u.Ops)
	}
	tr.apply(u)
	tr.advert["https://other.example/mcp"] = []string{"look"}
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "venues", Name: "other", URL: "https://other.example/mcp"}))
	rm := tr.render(VerbRemove, RemoveInput{Kind: "integration", Project: "venues", ID: "other"})
	if rm.Class == Refused || len(rm.Ops) != 1 || strings.Count(rm.Ops[0].Content, `transport: "sse"`) != 2 || strings.Contains(rm.Ops[0].Content, "other") {
		t.Fatalf("remove flipped the transport back: %s\n%+v", rm.Reason, rm.Ops)
	}
}

func TestAddMCPServer_DefaultStaysStreamableHTTP(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("venues")
	tr.advert["https://mcp.example/mcp"] = []string{"look"}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "venues", Name: "src", URL: "https://mcp.example/mcp"})
	tr.mustClass(c, Widening)
	if !strings.Contains(c.Ops[0].Content, `transport: "streamable-http"`) {
		t.Fatalf("the default transport is not streamable-http:\n%s", c.Ops[0].Content)
	}
	if c.Grant.Integrations[0].Transport != "" {
		t.Fatalf("the default must leave the grant byte-identical, got transport %q", c.Grant.Integrations[0].Transport)
	}
	if strings.Contains(c.Plain.Summary, "SSE") || !strings.Contains(c.Sentence, "over streamable HTTP") {
		t.Fatalf("summary %q sentence %q", c.Plain.Summary, c.Sentence)
	}
}

// Review 03b6 finding 1: an auth-none server that could not be reached is
// not told it needs a sign-in.
func TestAddMCPServer_UnreachableSentenceIsHonest(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("venues")
	tr.listFailed = map[string]string{scraperURL: "connection refused", "https://gated.example/mcp": "HTTP 401"}
	c := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "venues", Name: "scraper", URL: scraperURL})
	tr.mustClass(c, Widening)
	if strings.Contains(c.Sentence, "signing in") || !strings.Contains(c.Sentence, "could not be reached (connection refused)") {
		t.Fatalf("sentence: %s", c.Sentence)
	}
	g := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "venues", Name: "gated", URL: "https://gated.example/mcp"})
	if !strings.Contains(g.Sentence, "without signing in") {
		t.Fatalf("an auth-shaped failure lost the sign-in wording: %s", g.Sentence)
	}
}

// Review 03b6 item 5: a recipe reusing an existing SSE server renders that
// server's loaded transport, not the recipe's empty one.
func TestInstallRecipe_ReusedServerKeepsItsTransport(t *testing.T) {
	tr := probeTree(t)
	add := tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "personal", Name: "src", URL: probeURL, Transport: "sse"})
	tr.mustClass(add, Widening)
	tr.apply(add)
	v2 := strings.Replace(probeRecipe, "version: 1\nenvelope", "version: 2\nenvelope", 1)
	v2 = strings.Replace(v2, "read_tools: [look, peek]", "read_tools: [look, peek, scan]", 1)
	v2 = strings.Replace(v2, "tools: [file_write, mcp__src__look, mcp__src__peek]", "tools: [file_write, mcp__src__look, mcp__src__peek, mcp__src__scan]", 1)
	tr.r = rendererWith(t, "", map[string]string{"probe": v2})
	c := tr.render(VerbInstallRecipe, probeInstall(nil))
	if c.Class == Refused {
		t.Fatalf("refused: %s", c.Reason)
	}
	for _, op := range c.Ops {
		if strings.HasPrefix(op.Path, "projects/") {
			if !strings.Contains(op.Content, `transport: "sse"`) || strings.Contains(op.Content, "streamable-http") {
				t.Fatalf("the reused server lost its transport:\n%s", op.Content)
			}
			return
		}
	}
	t.Fatal("the install did not re-render the project")
}

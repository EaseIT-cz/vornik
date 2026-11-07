package dispatcher

// GitHub #642, scheduling bias (dispatcher turn-context design §7). Found at:
// production, 2026-11-04 22:35: after two failed scheduling attempts the
// dispatcher did a market study itself in about 30 web_fetch calls and hit
// the iteration cap.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

func TestResearchBudget_FifthCallRefused_642(t *testing.T) {
	g := newStatusReadGuard()
	for i := 1; i <= maxResearchCallsPerTurn; i++ {
		if _, refused := g.check("mcp__scraper__web_fetch", `{"url":"https://example.com/`+strings.Repeat("x", i)+`"}`); refused {
			t.Fatalf("call %d refused; the first %d run", i, maxResearchCallsPerTurn)
		}
	}
	msg, refused := g.check("mcp__consult__vornik_architecture", `{"q":"y"}`)
	if !refused || !strings.Contains(msg, "create_task") {
		t.Fatalf("the call past the budget must be refused toward create_task: %v %q", refused, msg)
	}
	// Other MCP tools are not research and stay unlimited.
	for i := 0; i < 10; i++ {
		if _, refused := g.check("mcp__pagedrop__pagedrop_list", "{}"); refused {
			t.Fatal("a non-research MCP tool must not be counted")
		}
	}
}

// The research count never resets within a turn (review 3cfe F4).
func TestResearchBudget_NoResetOnStateChange_642(t *testing.T) {
	g := newStatusReadGuard()
	for i := 0; i < maxResearchCallsPerTurn; i++ {
		g.check("mcp__scraper__web_fetch", `{"n":`+strings.Repeat("1", i+1)+`}`)
	}
	g.check("create_task", `{"prompt":"x"}`)
	if _, refused := g.check("mcp__scraper__web_fetch", `{"n":9}`); !refused {
		t.Fatal("a state-changing call must not re-arm the research budget")
	}
}

func TestResearchToolMatch_642(t *testing.T) {
	for name, want := range map[string]bool{
		"mcp__scraper__web_fetch":                  true,
		"mcp__x__web_search":                       true,
		"mcp__x__fetch_url":                        true,
		"mcp__x__scrape_page":                      true,
		"mcp__x__browse":                           true,
		"mcp__consult__vornik_architecture":        true,
		"mcp__scraper__ical_events":                false,
		"mcp__pagedrop__pagedrop_search":           false,
		"mcp__atlassian__searchJiraIssuesUsingJql": false,
		"memory_search":                            false,
	} {
		if got := isResearchTool(name); got != want {
			t.Errorf("isResearchTool(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPromptsSizeTheWorkFirst_642(t *testing.T) {
	for name, out := range map[string]string{
		"generic": BuildSystemPrompt("p1", nil),
		"lead":    BuildLeadSystemPrompt(&registry.Project{ID: "p1"}, nil, "You are the lead.", nil),
	} {
		i, j := strings.Index(out, sizeTheWorkBlock), strings.Index(out, "HOW TO HELP")
		if i < 0 || j < 0 || i > j {
			t.Fatalf("%s prompt: the size-the-work block must come before the ladder (at %d, ladder %d)", name, i, j)
		}
		if !strings.Contains(out, "your own services") {
			t.Fatalf("%s prompt: step 1 must be narrowed to the user's own services", name)
		}
		if strings.Contains(out, "ONLY when the above cannot help") {
			t.Fatalf("%s prompt: step 6 must agree with the sizing rule", name)
		}
		if !strings.Contains(sizeTheWorkBlock, "render_document") {
			t.Fatalf("the block must keep user-supplied rendering on render_document")
		}
	}
}

// Through the real loop: the model asks for web_fetch six times; four run.
func TestResearchBudgetInTheLoop_642(t *testing.T) {
	p := &toolSequenceProvider{tools: []string{"mcp__scraper__web_fetch", "mcp__scraper__web_fetch", "mcp__scraper__web_fetch",
		"mcp__scraper__web_fetch", "mcp__scraper__web_fetch", "mcp__scraper__web_fetch"}}
	mcp := &stubMCP{out: "page"}
	a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(10), WithMCPManager(mcp))
	res := a.Process(context.Background(), Request{Messages: []chat.Message{{Role: "user", Content: "research"}}, Project: "assistant"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	runs, refusals := 0, 0
	for _, m := range res.Messages {
		if m.Role != "tool" {
			continue
		}
		if strings.HasPrefix(m.Content, "Not run:") {
			refusals++
		} else {
			runs++
		}
	}
	if runs != maxResearchCallsPerTurn || refusals != 2 {
		t.Fatalf("runs=%d refusals=%d, want %d and 2", runs, refusals, maxResearchCallsPerTurn)
	}
}

// A research call does not reset the identical status-read count (review d088
// N1): fetching a page changes no task's state.
func TestResearchCallDoesNotResetStatusReads_642(t *testing.T) {
	g := newStatusReadGuard()
	g.check("get_task_status", `{"task_id":"x"}`)
	g.check("mcp__scraper__web_fetch", `{"url":"u"}`)
	g.check("get_task_status", `{"task_id":"x"}`)
	if _, refused := g.check("get_task_status", `{"task_id":"x"}`); !refused {
		t.Fatal("the third identical status read must be refused; a fetch is not a state change")
	}
}

// Failed research calls count like successful ones (review d088 N6).
func TestResearchBudgetCountsFailedCalls_642(t *testing.T) {
	tools := make([]string, 6)
	for i := range tools {
		tools[i] = "mcp__scraper__web_fetch"
	}
	p := &toolSequenceProvider{tools: tools}
	mcp := &stubMCP{err: errors.New("upstream timeout")}
	a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(10), WithMCPManager(mcp))
	res := a.Process(context.Background(), Request{Messages: []chat.Message{{Role: "user", Content: "research"}}, Project: "assistant"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	refusals := 0
	for _, m := range res.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "Not run:") {
			refusals++
		}
	}
	if refusals != 2 {
		t.Fatalf("failed calls must count: refusals = %d, want 2", refusals)
	}
}

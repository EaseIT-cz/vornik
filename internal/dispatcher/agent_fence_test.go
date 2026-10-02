package dispatcher

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/chat"
)

type recordingMCP struct{ calls []string }

func (r *recordingMCP) Tools(string) []chat.Tool {
	return []chat.Tool{{Type: "function", Function: chat.ToolFunction{Name: "mcp__bank__balance"}}}
}

func (r *recordingMCP) Execute(_ context.Context, projectID, name, _ string) (string, error) {
	r.calls = append(r.calls, projectID+":"+name)
	return "ok", nil
}

// Review 20261002-a048 F1 (agent-administered Vornik design §10.1): an agent
// project's integrations are approved for its own workflow roles, through
// the fail-closed gate on CallMCPTool. The operator's chat dispatcher has no
// task and no role, so it must never list or call them. Control:
// fenceAgentProjects, applied by every constructor that accepts an executor.
// Without it, a chat steered by untrusted input could call an agent's bank
// or mail integration with the agent's credentials.
func TestDispatcher_FencesAgentProjects(t *testing.T) {
	inner := &recordingMCP{}
	a := &Agent{}
	WithMCPManager(inner)(a)

	if got := a.mcpManager.Tools("hermes--fin"); len(got) != 0 {
		t.Fatalf("the chat was offered an agent project's tools: %+v", got)
	}
	if got := a.mcpManager.Tools("assistant"); len(got) != 1 {
		t.Fatalf("an operator project's tools were hidden: %+v", got)
	}
	_, err := a.mcpManager.Execute(context.Background(), "hermes--fin", "mcp__bank__balance", "{}")
	if err == nil || !strings.Contains(err.Error(), "agent project") {
		t.Fatalf("an agent project's tool ran from the chat: err=%v", err)
	}
	if _, err := a.mcpManager.Execute(context.Background(), "assistant", "mcp__bank__balance", "{}"); err != nil {
		t.Fatalf("an operator project's tool was refused: %v", err)
	}
	if len(inner.calls) != 1 || inner.calls[0] != "assistant:mcp__bank__balance" {
		t.Fatalf("calls reaching the executor: %v", inner.calls)
	}

	sw := NewMCPScraperWriteClient(inner, "s").(*mcpScraperWriteClient)
	if _, err := sw.exec.Execute(context.Background(), "hermes--fin", "mcp__scraper__web_submit", "{}"); err == nil {
		t.Fatal("the scraper write client called into an agent project")
	}
}

type countingAPI struct{ n int }

func (c *countingAPI) Call(context.Context, apigateway.Request) (apigateway.Response, error) {
	c.n++
	return apigateway.Response{Status: 200, Body: "{}"}, nil
}

// Plan P4.5: the chat's query_api never reaches an agent project's APIs,
// which are approved for the project's own workflows. Control: the agent
// branch of queryAPI.
func TestDispatcher_QueryAPIFencesAgentProjects(t *testing.T) {
	api := &countingAPI{}
	te := &ToolExecutor{apiClient: api}
	res := te.queryAPI(context.Background(), `{"provider":"fio","path":"x"}`, "hermes--fin", []string{"hermes--fin"})
	if !strings.Contains(res.Content, "agent project") || api.n != 0 {
		t.Fatalf("the chat queried an agent project's API: %q (%d calls)", res.Content, api.n)
	}
}

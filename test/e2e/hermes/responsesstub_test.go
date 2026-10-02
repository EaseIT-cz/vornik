package hermes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postResponses sends a Responses request and returns the streamed final
// output items.
func postResponses(t *testing.T, url string, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url+"/v1/responses", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var items []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev map[string]any
		_ = json.Unmarshal([]byte(line), &ev)
		if ev["type"] == "response.output_item.done" {
			items = append(items, ev["item"].(map[string]any))
		}
	}
	return items
}

// Agent-administered Vornik plan P8.4 (bring-up, 2026-10-02): Codex 0.153
// speaks only the Responses API, and calls an MCP tool by its plain name
// with the tool's namespace (mcp__<server>). The stub follows a script like
// LLMStub: with n function_call_output items it makes step n's call, then
// answers. Control: ResponsesStub.
func TestResponsesStub(t *testing.T) {
	stub := &ResponsesStub{Scripts: []Script{{Marker: "[A3-1]", Steps: []ScriptStep{
		{ToolSuffix: "codex__create_project", Args: map[string]any{"slug": "finance"}},
		{ToolSuffix: "codex__add_api", ArgsFrom: func(last string) map[string]any { return map[string]any{"after": last} }},
	}, FinalFrom: func(last string) string { return "done: " + last }}}}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	tools := []any{
		map[string]any{"type": "function", "name": "exec_command"},
		map[string]any{"type": "namespace", "name": "mcp__vornik_codex", "tools": []any{
			map[string]any{"type": "function", "name": "create_project"}, map[string]any{"type": "function", "name": "add_api"}}},
	}
	user := map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "[A3-1] set up"}}}

	items := postResponses(t, srv.URL, map[string]any{"stream": true, "tools": tools, "input": []any{user}})
	if len(items) != 1 || items[0]["type"] != "function_call" || items[0]["name"] != "create_project" || items[0]["namespace"] != "mcp__vornik_codex" {
		t.Fatalf("first turn: %v", items)
	}
	call := items[0]
	out := map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "created"}
	items = postResponses(t, srv.URL, map[string]any{"stream": true, "tools": tools, "input": []any{user, call, out}})
	if len(items) != 1 || items[0]["name"] != "add_api" || !strings.Contains(items[0]["arguments"].(string), `"after":"created"`) {
		t.Fatalf("second turn: %v", items)
	}
	out2 := map[string]any{"type": "function_call_output", "call_id": items[0]["call_id"], "output": "added"}
	items = postResponses(t, srv.URL, map[string]any{"stream": true, "tools": tools, "input": []any{user, call, out, items[0], out2}})
	if len(items) != 1 || items[0]["type"] != "message" || !strings.Contains(fmt.Sprint(items[0]), "done: added") {
		t.Fatalf("final turn: %v", items)
	}
	if len(stub.Failures()) != 0 {
		t.Fatalf("failures: %v", stub.Failures())
	}
}

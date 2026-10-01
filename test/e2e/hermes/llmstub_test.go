package hermes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func complete(t *testing.T, url string, msgs []map[string]any, tools []string) map[string]any {
	t.Helper()
	var ts []map[string]any
	for _, n := range tools {
		ts = append(ts, map[string]any{"type": "function", "function": map[string]any{"name": n}})
	}
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": msgs, "tools": ts})
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
}

func TestLLMStub_FollowsTheScriptThenAnswers(t *testing.T) {
	stub := &LLMStub{Steps: []ScriptStep{
		{ToolSuffix: "gmail_search", Args: map[string]any{"q": "newer_than:1d"}},
		{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/digest.json"}},
	}, Final: "done"}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	tools := []string{"mcp__mail-read__gmail_search", "file_write"}
	user := []map[string]any{{"role": "user", "content": "go"}}

	m := complete(t, srv.URL, user, tools)
	call := m["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if call["name"] != "mcp__mail-read__gmail_search" || !strings.Contains(call["arguments"].(string), "newer_than") {
		t.Fatalf("step 0 = %v", call)
	}
	m = complete(t, srv.URL, append(user, map[string]any{"role": "tool", "content": "x"}), tools)
	if m["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] != "file_write" {
		t.Fatalf("step 1 = %v", m)
	}
	m = complete(t, srv.URL, append(user, map[string]any{"role": "tool"}, map[string]any{"role": "tool"}), tools)
	if m["content"] != "done" || m["tool_calls"] != nil {
		t.Fatalf("final = %v", m)
	}
	if got := stub.ToolsSeen(); len(got) != 2 || got[0] != "file_write" {
		t.Fatalf("tools seen = %v", got)
	}
	if stub.Requests() != 3 || len(stub.Failures()) != 0 {
		t.Fatalf("requests=%d failures=%v", stub.Requests(), stub.Failures())
	}
}

// A step whose tool was not offered is recorded, so the lane can fail with
// the reason instead of a model that silently answered in prose.
func TestLLMStub_MissingToolIsAFailure(t *testing.T) {
	stub := &LLMStub{Steps: []ScriptStep{{ToolSuffix: "gmail_send"}}}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	complete(t, srv.URL, []map[string]any{{"role": "user"}}, []string{"file_write"})
	if f := stub.Failures(); len(f) != 1 || !strings.Contains(f[0], "gmail_send") {
		t.Fatalf("failures = %v", f)
	}
}

// Scripts are chosen per conversation by a marker in its text, so one stub
// serves both broker workflows (and the hostile variant, H2b).
func TestLLMStub_ChoosesAScriptByMarker(t *testing.T) {
	stub := &LLMStub{Final: "default", Scripts: []Script{
		{Marker: "Draft a reply", Steps: []ScriptStep{{ToolSuffix: "gmail_get"}}, Final: "reply done"},
		{Marker: "Build a digest", Final: "digest done"},
	}}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	m := complete(t, srv.URL, []map[string]any{{"role": "system", "content": "Draft a reply to one message"}}, []string{"mcp__gw__gmail_get"})
	if m["tool_calls"] == nil {
		t.Fatalf("reply script not chosen: %v", m)
	}
	m = complete(t, srv.URL, []map[string]any{{"role": "user", "content": "Build a digest of the mail"}}, nil)
	if m["content"] != "digest done" {
		t.Fatalf("digest script not chosen: %v", m)
	}
	m = complete(t, srv.URL, []map[string]any{{"role": "user", "content": "anything else"}}, nil)
	if m["content"] != "default" {
		t.Fatalf("fallback = %v", m)
	}
}

// Embeddings are deterministic, of the configured dimension, and differ by
// input, so the memory pipeline runs without a model.
func TestLLMStub_Embeddings(t *testing.T) {
	stub := &LLMStub{EmbeddingDim: 8}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	get := func(input any) [][]any {
		body, _ := json.Marshal(map[string]any{"model": "e", "input": input})
		resp, err := http.Post(srv.URL+"/v1/embeddings", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Data []struct {
				Embedding []any `json:"embedding"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		var vs [][]any
		for _, d := range out.Data {
			vs = append(vs, d.Embedding)
		}
		return vs
	}
	a, b := get("dentist"), get([]string{"dentist", "invoice"})
	if len(a) != 1 || len(a[0]) != 8 || len(b) != 2 {
		t.Fatalf("shapes: %d %d", len(a), len(b))
	}
	if fmt.Sprint(a[0]) != fmt.Sprint(b[0]) || fmt.Sprint(b[0]) == fmt.Sprint(b[1]) {
		t.Fatal("embeddings must be deterministic per input and differ across inputs")
	}
}

// A step can build its arguments from the previous tool result (a task id
// returned by delegate), and the final reply can quote it.
func TestLLMStub_DynamicArgsAndFinal(t *testing.T) {
	stub := &LLMStub{Scripts: []Script{{Marker: "go",
		Steps: []ScriptStep{
			{ToolSuffix: "delegate", Args: map[string]any{"workflow": "w"}},
			{ToolSuffix: "result", ArgsFrom: func(last string) map[string]any { return map[string]any{"task_id": last} }},
		},
		FinalFrom: func(last string) string { return "got " + last },
	}}}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	tools := []string{"vornik_delegate", "vornik_result"}
	m := complete(t, srv.URL, []map[string]any{{"role": "user", "content": "go"}, {"role": "tool", "content": "task_42"}}, tools)
	args := m["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(args, "task_42") {
		t.Fatalf("args = %s", args)
	}
	m = complete(t, srv.URL, []map[string]any{{"role": "user", "content": "go"}, {"role": "tool", "content": "a"}, {"role": "tool", "content": "done-7"}}, tools)
	if m["content"] != "got done-7" {
		t.Fatalf("final = %v", m["content"])
	}
}

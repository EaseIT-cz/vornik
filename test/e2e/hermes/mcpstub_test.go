package hermes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func rpc(t *testing.T, url, method string, params any, id int) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMCPStub_ListsCallsAndRecords(t *testing.T) {
	stub := NewMCPStub("mail-send", MCPTool{Name: "gmail_send", Handle: func(json.RawMessage) (string, bool) {
		return `{"sent":true}`, false
	}})
	srv := httptest.NewServer(stub)
	defer srv.Close()

	if got := rpc(t, srv.URL, "initialize", map[string]any{}, 1); got["result"] == nil {
		t.Fatalf("initialize: %v", got)
	}
	list := rpc(t, srv.URL, "tools/list", map[string]any{}, 2)["result"].(map[string]any)["tools"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "gmail_send" {
		t.Fatalf("tools = %v", list)
	}
	res := rpc(t, srv.URL, "tools/call", map[string]any{"name": "gmail_send", "arguments": map[string]any{"to": "a@b.c"}}, 3)["result"].(map[string]any)
	if res["isError"] != false {
		t.Fatalf("call result = %v", res)
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].Tool != "gmail_send" || string(calls[0].Args) != `{"to":"a@b.c"}` {
		t.Fatalf("calls = %+v", calls)
	}
	if res := rpc(t, srv.URL, "tools/call", map[string]any{"name": "nope"}, 4)["result"].(map[string]any); res["isError"] != true {
		t.Fatalf("unknown tool must be a tool error: %v", res)
	}
	if got := rpc(t, srv.URL, "bogus", nil, 5); got["error"] == nil {
		t.Fatal("unknown method must be a JSON-RPC error")
	}
}

func TestMCPStub_NotificationsGetNoBody(t *testing.T) {
	srv := httptest.NewServer(NewMCPStub("x"))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

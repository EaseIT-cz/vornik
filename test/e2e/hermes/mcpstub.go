// Package hermes is the self-contained end-to-end lane for the Hermes
// companion plugin — https://docs.vornik.io
// The stubs in this file and llmstub.go build without the e2e_hermes tag, so
// their unit tests run in the ordinary lane; the scenarios need the tag.
package hermes

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// MCPTool is one tool a stub MCP server offers.
type MCPTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Handle returns the tool's text result, or isError for a tool error.
	Handle func(args json.RawMessage) (text string, isError bool)
}

// MCPCall is one recorded tools/call.
type MCPCall struct {
	Tool string
	Args json.RawMessage
}

// MCPStub is a minimal MCP server over streamable HTTP with JSON replies:
// initialize, notifications, tools/list and tools/call. It records every
// call, which is how the lane counts sends.
type MCPStub struct {
	Name  string
	tools []MCPTool
	mu    sync.Mutex
	calls []MCPCall
}

// NewMCPStub builds a stub server offering tools.
func NewMCPStub(name string, tools ...MCPTool) *MCPStub {
	return &MCPStub{Name: name, tools: tools}
}

// Calls returns the recorded tools/call requests, in order.
func (s *MCPStub) Calls() []MCPCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MCPCall(nil), s.calls...)
}

// ServeHTTP implements the JSON-RPC endpoint.
func (s *MCPStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Params json.RawMessage  `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.ID == nil { // a notification: no reply body
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "stub-"+s.Name)
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": "e2e"},
		}
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
		}
		result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		s.calls = append(s.calls, MCPCall{Tool: p.Name, Args: p.Arguments})
		s.mu.Unlock()
		text, isErr := "unknown tool "+p.Name, true
		for _, t := range s.tools {
			if t.Name == p.Name {
				text, isErr = t.Handle(p.Arguments)
			}
		}
		result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
	default:
		writeRPC(w, req.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
		return
	}
	writeRPC(w, req.ID, result, nil)
}

func writeRPC(w http.ResponseWriter, id *json.RawMessage, result, rpcErr any) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

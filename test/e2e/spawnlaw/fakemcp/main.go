//go:build e2e_http

// Command fakemcp is a minimal MCP server for the process-spawn-law end-to-end
// suite: it answers initialize and tools/list with one tool, "echo", over stdio
// (the default) or streamable HTTP (-http addr). Nothing else.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
}

// answer returns the JSON-RPC response for a request, or nil for a
// notification (no id), which gets no response.
func answer(req request) []byte {
	if len(req.ID) == 0 {
		return nil
	}
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fakemcp", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name":        "echo",
			"description": "Echo the input back.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		}}}
	default:
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": "method not found"}})
		return out
	}
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	return out
}

func main() {
	addr := flag.String("http", "", "serve streamable HTTP on this address instead of stdio")
	flag.Parse()
	if *addr == "" {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var req request
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			if out := answer(req); out != nil {
				fmt.Println(string(out))
			}
		}
		return
	}
	http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req request
		if json.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		out := answer(req)
		if out == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "fake-session")
		_, _ = w.Write(out)
	})
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

package hermes

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// fakeStdioServer answers newline JSON-RPC: tools/call returns its tool name
// as text, an unknown method an error; a notification gets nothing.
func fakeStdioServer(in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(sc.Bytes(), &req)
		if len(req.ID) == 0 {
			continue
		}
		var resp map[string]any
		switch req.Method {
		case "tools/call":
			// A server notification first: the client must skip it.
			_, _ = io.WriteString(out, `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`+"\n")
			resp = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ran " + req.Params.Name}}, "isError": req.Params.Name == "bad"}}
		case "initialize":
			resp = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"instructions": "be good"}}
		default:
			resp = map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "nope"}}
		}
		b, _ := json.Marshal(resp)
		_, _ = out.Write(append(b, '\n'))
	}
}

// Agent-administered Vornik plan P8.2: the lane's MCP stdio client (Claude
// Desktop's stand-in) matches replies to requests by id, skips server
// notifications, and keeps a transcript of everything it sent and read for
// the canary sweep. Control: StdioClient.
func TestStdioClient(t *testing.T) {
	cin, sin := io.Pipe()
	sout, cout := io.Pipe()
	go fakeStdioServer(cin, cout)
	c := NewStdioClient(sin, sout)
	res, err := c.Call("initialize", map[string]any{})
	if err != nil || !strings.Contains(string(res), "be good") {
		t.Fatalf("initialize: %s %v", res, err)
	}
	text, isErr, err := c.Tool("describe_installation", map[string]any{})
	if err != nil || isErr || text != "ran describe_installation" {
		t.Fatalf("tool: %q %v %v", text, isErr, err)
	}
	if _, isErr, _ := c.Tool("bad", nil); !isErr {
		t.Fatal("a tool error was not reported")
	}
	if _, err := c.Call("bogus", nil); err == nil {
		t.Fatal("a JSON-RPC error was not reported")
	}
	if !strings.Contains(c.Transcript(), `"method":"tools/call"`) || !strings.Contains(c.Transcript(), "ran bad") {
		t.Fatalf("transcript: %s", c.Transcript())
	}
}

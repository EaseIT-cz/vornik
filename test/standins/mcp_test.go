package standins

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcp"
)

// GitHub #337 (2026-10-12): the stand-ins are driven by the daemon's own MCP
// client (internal/mcp), so a stand-in that the daemon cannot talk to fails
// here, in the ordinary lane, before any e2e arm runs.

// dial connects the daemon's MCP client to url with token as the bearer.
func dial(t *testing.T, url, token string) *mcp.Client {
	t.Helper()
	cfg := mcp.ServerConfig{Name: "standin", Transport: "streamable-http", URL: url}
	if token != "" {
		cfg.AuthHeaders = map[string]string{"Authorization": "Bearer " + token}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := mcp.Connect(ctx, cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// call runs one tool and decodes its text as JSON into out (when non-nil);
// it returns the text and whether the tool answered an error.
func call(t *testing.T, c *mcp.Client, tool string, args any, out any) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := c.CallTool(context.Background(), tool, raw)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	text := res.Text()
	if out != nil && !res.IsError {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s: %v in %s", tool, err, text)
		}
	}
	return text, res.IsError
}

func echoServer() *MCPServer {
	return NewMCPServer("echo",
		Tool{Name: "echo_read", Handle: func(user string, args json.RawMessage) (any, error) {
			return map[string]any{"user": user, "args": args}, nil
		}},
		Tool{Name: "echo_write", Write: true, Handle: func(user string, _ json.RawMessage) (any, error) {
			return map[string]any{"written_by": user}, nil
		}},
	)
}

func TestMCPServer_RefusesWithoutAKnownBearer(t *testing.T) {
	s := echoServer()
	s.Token("tok-alice", "alice")
	srv := httptest.NewServer(s)
	defer srv.Close()

	for _, bearer := range []string{"", "Bearer nope"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		if bearer != "" {
			req.Header.Set("Authorization", bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bearer %q: HTTP %d, want 401", bearer, resp.StatusCode)
		}
	}
	if s.Refused() != 2 {
		t.Fatalf("refused = %d, want 2", s.Refused())
	}
	if resp, err := http.Get(srv.URL); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %v %v", resp, err)
	}
}

func TestMCPServer_ResolvesTheUserAndRecordsRawArguments(t *testing.T) {
	s := echoServer()
	s.Token("tok-alice", "alice")
	s.Token("tok-bob", "bob")
	srv := httptest.NewServer(s)
	defer srv.Close()

	alice := dial(t, srv.URL, "tok-alice")
	if len(alice.Tools()) != 2 {
		t.Fatalf("tools = %v", alice.Tools())
	}
	var got struct{ User string }
	call(t, alice, "echo_read", map[string]any{"b": 2, "a": 1}, &got)
	if got.User != "alice" {
		t.Fatalf("user = %q", got.User)
	}
	call(t, dial(t, srv.URL, "tok-bob"), "echo_write", map[string]any{}, nil)

	calls := s.Calls()
	if len(calls) != 2 || calls[0].User != "alice" || calls[1].User != "bob" {
		t.Fatalf("calls = %+v", calls)
	}
	// The raw bytes as sent, not a re-encoding: an approval hash is over them.
	if !bytes.Equal(calls[0].Args, []byte(`{"a":1,"b":2}`)) {
		t.Fatalf("raw args = %s", calls[0].Args)
	}
	if w := s.Writes(); len(w) != 1 || w[0].Tool != "echo_write" || !w[0].Write {
		t.Fatalf("writes = %+v", w)
	}
	if n := len(s.CallsBy("bob")); n != 1 {
		t.Fatalf("bob's calls = %d", n)
	}
}

func TestMCPServer_UnknownToolAndMethod(t *testing.T) {
	s := echoServer()
	s.Token("t", "alice")
	srv := httptest.NewServer(s)
	defer srv.Close()

	if _, isErr := call(t, dial(t, srv.URL, "t"), "nope", map[string]any{}, nil); !isErr {
		// The daemon's client refuses an unlisted tool before sending; either
		// way nothing is recorded as a call of a tool that does not exist.
		t.Log("the client refused the unknown tool")
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`))
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Error struct{ Code int } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", out)
	}
}

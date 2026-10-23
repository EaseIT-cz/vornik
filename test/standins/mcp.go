// Package standins holds stateful stand-ins for the services Vornik's
// recipes and connections reach (mail, calendar, a CRM, an OAuth provider,
// IMAP and SMTP), so the e2e lanes run the recipe path end to end with no
// real account and no network egress. Hermes e2e lane design, amendment
// 2026-10-12 (GitHub #337), owns them.
//
// Every server listens on loopback only and holds several users' data; the
// credential a request carries selects whose. Every MCP tools/call is
// recorded with its user, its tool and its raw argument bytes.
//
// They are fixtures, not providers. What the mail and calendar servers DO
// imitate (GitHub #404, 2026-10-17): the read tools of the reference Google
// Workspace server (gemini-cli workspace extension 089927e,
// workspace-server/src/index.ts) by their argument names, gmail_search
// {query, maxResults, pageToken, labelIds, includeSpamTrash}, gmail_get
// {messageId, format} and calendar.listEvents {calendarId (required),
// timeMin, timeMax, attendeeResponseStatus, eventTypes}; and their tool
// names, except that the calendar tool keeps the recipes' spelling
// calendar_list_events where the reference's is calendar_listEvents. An
// argument a stand-in cannot honour is refused as invalid_argument, never
// ignored. The writes (gmail_send, calendar_create_event) imitate no
// reference shape.
//
// What each does NOT imitate, so a green lane is not read as "verified
// against the real provider":
//
//   - mail MCP: Gmail's full query grammar, labels other than INBOX and
//     UNREAD, spam and trash, threads, pagination (any pageToken is refused),
//     MIME parts and attachments, the raw format's RFC 2822 bytes, quota and
//     rate-limit answers (429), the real server's result shape beyond the
//     fields the recipes name.
//   - calendar MCP: several calendars (every calendarId names the user's
//     one), recurring events, time zones other than UTC, event types other
//     than default, free/busy, attendees' responses, Google Calendar's REST
//     API or CalDAV.
//   - CRM MCP: any real CRM's schema, search semantics or permissions.
//   - OAuth: a login or consent page, scopes enforced per tool, token
//     revocation, signed id_tokens and JWKS, provider error pages, expiry
//     shorter than a lane run.
//   - IMAP: XOAUTH2, real providers' IDLE behaviour, Gmail's IMAP
//     extensions, server-side quotas; a \Seen set over IMAP is not
//     reflected in the MCP view's is:unread.
//   - SMTP: STARTTLS, DKIM/SPF, bounces, size limits, relaying.
package standins

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
)

// Tool is one tool an MCP stand-in offers. Handle gets the caller's user
// (resolved from the bearer) and the raw arguments; an error becomes a tool
// error (isError) carrying its message.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Write marks a tool that changes the service, so a test counts writes
	// apart from reads.
	Write  bool
	Handle func(user string, args json.RawMessage) (any, error)
}

// Call is one recorded tools/call.
type Call struct {
	User  string
	Tool  string
	Args  json.RawMessage // the bytes as sent, never re-encoded
	Write bool
	// Err is the tool error the call was answered with, empty when it
	// succeeded (GitHub #404: a lane that never looked at it passed with
	// arguments the stand-in refused).
	Err string
}

// MCPServer is an MCP server over streamable HTTP with JSON replies.
type MCPServer struct {
	Name    string
	tools   []Tool
	oauth   *OAuthServer
	mu      sync.Mutex
	tokens  map[string]string
	calls   []Call
	refused int
}

// NewMCPServer builds a server offering tools. It answers nobody until a
// token is added or it is protected by an OAuth server.
func NewMCPServer(name string, tools ...Tool) *MCPServer {
	return &MCPServer{Name: name, tools: tools, tokens: map[string]string{}}
}

// Token makes bearer token act as user.
func (s *MCPServer) Token(token, user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = user
}

// Protect puts the server behind an OAuth server: its access tokens act as
// their user, and a refused request's challenge names its metadata.
func (s *MCPServer) Protect(o *OAuthServer) { s.oauth = o }

// Calls returns every recorded tools/call, in order.
func (s *MCPServer) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallsBy returns the calls made with user's credential.
func (s *MCPServer) CallsBy(user string) []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.User == user {
			out = append(out, c)
		}
	}
	return out
}

// Writes returns the recorded calls of write tools, refused ones included.
func (s *MCPServer) Writes() []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Write {
			out = append(out, c)
		}
	}
	return out
}

// Failed returns the recorded calls answered with a tool error (a misnamed
// or invalid argument, a message that is not the caller's, ...), in order.
func (s *MCPServer) Failed() []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Err != "" {
			out = append(out, c)
		}
	}
	return out
}

// Refused counts requests refused for a missing or unknown bearer.
func (s *MCPServer) Refused() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

func (s *MCPServer) user(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || h[:len(p)] != p {
		return "", false
	}
	tok := h[len(p):]
	s.mu.Lock()
	u, ok := s.tokens[tok]
	s.mu.Unlock()
	if ok {
		return u, true
	}
	if s.oauth != nil {
		return s.oauth.UserFor(tok)
	}
	return "", false
}

// ServeHTTP implements the JSON-RPC endpoint.
func (s *MCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	user, ok := s.user(r)
	if !ok {
		s.mu.Lock()
		s.refused++
		s.mu.Unlock()
		if s.oauth != nil {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+baseURL(r)+prmPath+r.URL.Path+`"`)
		}
		w.WriteHeader(http.StatusUnauthorized)
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
	if req.ID == nil { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "standin-"+s.Name)
		result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": "standin"},
		}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object"}
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
		result = s.callTool(user, p.Name, p.Arguments)
	default:
		writeRPC(w, req.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
		return
	}
	writeRPC(w, req.ID, result, nil)
}

func (s *MCPServer) callTool(user, name string, args json.RawMessage) any {
	text, isErr := "unknown tool "+name, true
	for _, t := range s.tools {
		if t.Name != name {
			continue
		}
		call := Call{User: user, Tool: name, Args: append(json.RawMessage(nil), args...), Write: t.Write}
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage(`{}`)
		}
		out, err := t.Handle(user, args)
		if err != nil {
			text = err.Error()
			call.Err = text
		} else {
			b, _ := json.Marshal(out)
			text, isErr = string(b), false
		}
		s.mu.Lock()
		s.calls = append(s.calls, call)
		s.mu.Unlock()
		break
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
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

// baseURL is the origin the request reached (stand-ins serve plain HTTP on
// loopback), so metadata documents name the port the test chose.
func baseURL(r *http.Request) string { return "http://" + r.Host }

// decode unmarshals a tool's arguments, refusing unknown fields so a
// caller's misnamed argument is an error rather than silently ignored.
//
// The daemon adds its own project_id to every MCP call; it is accepted and
// ignored (A9's first local run, 2026-10-12, found the approved write
// refused for it).
func decode(args json.RawMessage, v any) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) == nil {
		if _, ok := m["project_id"]; ok {
			delete(m, "project_id")
			args, _ = json.Marshal(m)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid arguments: " + err.Error())
	}
	return nil
}

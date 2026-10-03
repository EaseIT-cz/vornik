package agentbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Agent-administered design §18.14 finding 2 (round 2 and review be5a
// govern, GREEN): a harness connected before a change holds the old tool
// list. The bridge, which runs for the whole session, lists the tools
// itself in the harness's session every minute after the harness's
// initialize, hashes them, seeds the hash from the harness's own first
// tools/list reply, and writes notifications/tools/list_changed when they
// change. One writer on stdout; its own client with a 5 second timeout; a
// failed poll changes nothing, logs without the key, and cannot stop the
// relay.

const listChanged = `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`

// pollServer is a companion endpoint whose tool list a test can change.
type pollServer struct {
	mu          sync.Mutex
	tools       string // the tools array, JSON
	alternate   bool   // every tools/list after the first answers a different list
	failLists   bool   // tools/list after the harness's first answers 500
	delay       time.Duration
	lists       int // tools/list requests seen
	inflight    int
	maxInflight int
	sessions    []string // Mcp-Session-Id of each tools/list
	versions    []string // MCP-Protocol-Version of each tools/list
	sseEvents   int
	// paged answers one tool per tools/list page, the cursor being the
	// next tool's index (review 195e F2).
	paged bool
	// initVersions are the protocolVersions successive initialize results
	// carry; empty is always 2025-06-18 (review 195e F4).
	initVersions []string
}

func (p *pollServer) setTools(s string) {
	p.mu.Lock()
	p.tools = s
	p.mu.Unlock()
}

func (p *pollServer) listCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lists
}

func (p *pollServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-1")
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			version := "2025-06-18"
			p.mu.Lock()
			if len(p.initVersions) > 0 {
				version, p.initVersions = p.initVersions[0], p.initVersions[1:]
			}
			p.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{"tools":{"listChanged":true}}}}`, req.ID, version)
		case "tools/list":
			p.mu.Lock()
			p.lists++
			n := p.lists
			p.inflight++
			if p.inflight > p.maxInflight {
				p.maxInflight = p.inflight
			}
			p.sessions = append(p.sessions, r.Header.Get("Mcp-Session-Id"))
			p.versions = append(p.versions, r.Header.Get("MCP-Protocol-Version"))
			tools, fail, delay, paged := p.tools, p.failLists && n > 1, p.delay, p.paged
			if p.alternate && n > 1 && n%2 == 0 {
				tools = `[{"name":"list_my_setup","inputSchema":{"type":"object"}},{"name":"poll_` + fmt.Sprint(n) + `"}]`
			}
			p.mu.Unlock()
			defer func() {
				p.mu.Lock()
				p.inflight--
				p.mu.Unlock()
			}()
			if n > 1 && delay > 0 {
				time.Sleep(delay)
			}
			if fail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			next := ""
			if paged {
				var all []json.RawMessage
				_ = json.Unmarshal([]byte(tools), &all)
				i := 0
				_, _ = fmt.Sscan(req.Params.Cursor, &i)
				tools = "[" + string(all[i]) + "]"
				if i+1 < len(all) {
					next = fmt.Sprintf(`,"nextCursor":"%d"`, i+1)
				}
			}
			// An SSE reply, as the companion endpoint may answer.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":%s%s}}\n\n", req.ID, tools, next)
		case "sse":
			w.Header().Set("Content-Type", "text/event-stream")
			p.mu.Lock()
			n := p.sseEvents
			p.mu.Unlock()
			for i := 0; i < n; i++ {
				_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":%d,\"message\":\"%s\"}}\n\n", i, strings.Repeat("x", 200))
				w.(http.Flusher).Flush()
				time.Sleep(200 * time.Microsecond)
			}
			_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", req.ID)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, req.ID)
		}
	})
}

// lineBuf is a concurrency-safe output that can be asked for its lines.
// With slow set it takes each Write a byte at a time, yielding between
// bytes, so two unsynchronised writers would interleave.
type lineBuf struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	slow bool
}

func (b *lineBuf) Write(p []byte) (int, error) {
	if !b.slow {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.buf.Write(p)
	}
	for _, c := range p {
		b.mu.Lock()
		b.buf.WriteByte(c)
		b.mu.Unlock()
		runtime.Gosched()
	}
	return len(p), nil
}

func (b *lineBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lineBuf) lines() []string {
	s := strings.TrimRight(b.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (b *lineBuf) notifications() int {
	n := 0
	for _, l := range b.lines() {
		if l == listChanged {
			n++
		}
	}
	return n
}

// session is one bridge run with an open stdin.
type session struct {
	t        *testing.T
	in       *io.PipeWriter
	out, err *lineBuf
	done     chan error
}

func startSession(t *testing.T, endpoint string, every time.Duration, slow bool) *session {
	t.Helper()
	return startSessionWith(t, Config{Endpoint: endpoint, Key: testKey, PollEvery: every}, slow)
}

func startSessionWith(t *testing.T, cfg Config, slow bool) *session {
	t.Helper()
	pr, pw := io.Pipe()
	s := &session{t: t, in: pw, out: &lineBuf{slow: slow}, err: &lineBuf{}, done: make(chan error, 1)}
	go func() {
		s.done <- Run(context.Background(), pr, s.out, s.err, cfg)
	}()
	t.Cleanup(func() { _ = pw.Close() })
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) close() {
	s.t.Helper()
	_ = s.in.Close()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		s.t.Fatal("the bridge did not stop when stdin closed")
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// handshake relays initialize, initialized and the harness's own first
// tools/list, and waits for its reply.
func (s *session) handshake() {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	waitFor(s.t, "the harness's tools/list reply", func() bool {
		for _, l := range s.out.lines() {
			if strings.Contains(l, `"id":2`) {
				return true
			}
		}
		return false
	})
}

const toolsV1 = `[{"name":"list_my_setup","inputSchema":{"type":"object","properties":{}}}]`

// §18.14 round 2 F5 and review be5a: a change after the harness listed the
// tools gives exactly one list_changed; the bridge's own lists ride the
// harness's session and protocol version; the hash is seeded from the
// harness's reply (the change lands before the first poll, so a hash seeded
// from the first poll would never notify).
func TestBridgePoll_OneNotificationPerChange(t *testing.T) {
	ps := &pollServer{tools: toolsV1}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	s := startSession(t, srv.URL, 40*time.Millisecond, false)
	s.handshake()
	ps.setTools(`[{"name":"list_my_setup","inputSchema":{"type":"object","properties":{}}},{"name":"define_swarm"}]`)
	waitFor(t, "a list_changed", func() bool { return s.out.notifications() >= 1 })
	before := ps.listCount()
	waitFor(t, "three more polls", func() bool { return ps.listCount() >= before+3 })
	s.close()
	if n := s.out.notifications(); n != 1 {
		t.Fatalf("list_changed written %d times for one change: %q", n, s.out.lines())
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for i := 1; i < len(ps.sessions); i++ {
		if ps.sessions[i] != "sess-1" || ps.versions[i] != "2025-06-18" {
			t.Fatalf("poll %d sent session %q version %q", i, ps.sessions[i], ps.versions[i])
		}
	}
	for _, l := range s.out.lines() {
		if strings.Contains(l, "vornik-bridge") {
			t.Fatalf("a poll reply reached the harness: %q", l)
		}
	}
}

// §18.14: an unchanged list never notifies, however many polls run. The
// denominator is the poll count.
func TestBridgePoll_UnchangedIsSilent(t *testing.T) {
	ps := &pollServer{tools: toolsV1}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	s := startSession(t, srv.URL, 5*time.Millisecond, false)
	s.handshake()
	waitFor(t, "five polls", func() bool { return ps.listCount() >= 6 })
	s.close()
	if n := s.out.notifications(); n != 0 {
		t.Fatalf("list_changed for an unchanged list after %d polls", ps.listCount()-1)
	}
	if len(s.out.lines()) != 2 {
		t.Fatalf("stdout carries more than the two replies: %q", s.out.lines())
	}
}

// §18.14 round 2 F2: a failed read changes nothing (no notification even
// though the list did change), logs a line on stderr without the key, and
// the relay keeps answering.
func TestBridgePoll_FailedReadChangesNothing(t *testing.T) {
	ps := &pollServer{tools: toolsV1, failLists: true}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	s := startSession(t, srv.URL, 5*time.Millisecond, false)
	s.handshake()
	ps.setTools(`[{"name":"changed"}]`)
	waitFor(t, "three failed polls", func() bool { return ps.listCount() >= 4 })
	s.send(`{"jsonrpc":"2.0","id":9,"method":"tools/call"}`)
	waitFor(t, "the relay's answer after failed polls", func() bool { return strings.Contains(s.out.String(), `"id":9`) })
	s.close()
	if n := s.out.notifications(); n != 0 {
		t.Fatalf("a failed read notified %d times", n)
	}
	if !strings.Contains(s.err.String(), "tools poll") {
		t.Fatalf("a failed poll is not said on stderr: %q", s.err.String())
	}
	if strings.Contains(s.out.String()+s.err.String(), testKey) || strings.Contains(s.err.String(), "CANARYKEY") {
		t.Fatal("the key reached stdout or stderr")
	}
}

// §18.14 round 2 F3: no poll and no notification before the harness's
// initialize result.
func TestBridgePoll_NothingBeforeInitialize(t *testing.T) {
	ps := &pollServer{tools: toolsV1}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	s := startSession(t, srv.URL, 2*time.Millisecond, false)
	time.Sleep(60 * time.Millisecond) // thirty intervals with no initialize
	ps.setTools(`[{"name":"changed"}]`)
	time.Sleep(30 * time.Millisecond)
	s.close()
	if n := ps.listCount(); n != 0 {
		t.Fatalf("%d polls before initialize", n)
	}
	if out := s.out.String(); out != "" {
		t.Fatalf("stdout before initialize: %q", out)
	}
}

// §18.14 round 2 F2: a tick is skipped while a poll is in flight.
func TestBridgePoll_OneInFlight(t *testing.T) {
	ps := &pollServer{tools: toolsV1, delay: 30 * time.Millisecond}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	s := startSession(t, srv.URL, 2*time.Millisecond, false)
	s.handshake()
	waitFor(t, "three slow polls", func() bool { return ps.listCount() >= 4 })
	s.close()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.maxInflight != 1 {
		t.Fatalf("%d tools/list in flight at once", ps.maxInflight)
	}
}

// §18.14 round 2 F2: the poll's own client, with a 5 second timeout,
// never the relay's (whose calls may run for minutes).
func TestBridgePoll_OwnClientWithShortTimeout(t *testing.T) {
	relay := &http.Client{Timeout: 10 * time.Minute}
	c := newPollClient()
	if c == relay || c == http.DefaultClient || c.Timeout != 5*time.Second {
		t.Fatalf("poll client %p (relay %p), timeout %v", c, relay, c.Timeout)
	}
}

// §18.14 round 2 F2: a poll that panics is recovered, said on stderr, and
// the relay goes on.
func TestBridgePoll_PanicDoesNotStopTheRelay(t *testing.T) {
	ps := &pollServer{tools: toolsV1}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	var once sync.Once
	panicked := make(chan struct{})
	// Review 195e F3: the seam is a field of this run, not a package var
	// the poll goroutine reads unsynchronised.
	s := startSessionWith(t, Config{Endpoint: srv.URL, Key: testKey, PollEvery: 2 * time.Millisecond, pollHook: func() {
		once.Do(func() {
			close(panicked)
			panic("a poll defect")
		})
	}}, false)
	s.handshake()
	<-panicked
	waitFor(t, "a poll after the panic", func() bool { return ps.listCount() >= 2 })
	s.send(`{"jsonrpc":"2.0","id":9,"method":"tools/call"}`)
	waitFor(t, "the relay's answer after the panic", func() bool { return strings.Contains(s.out.String(), `"id":9`) })
	s.close()
	if !strings.Contains(s.err.String(), "tools poll") {
		t.Fatalf("the recovered panic is not said on stderr: %q", s.err.String())
	}
}

// §18.14 round 2 F1 and F8: a relayed SSE reply and the poll's
// notifications written at the same time leave stdout well-formed
// newline-delimited JSON. The output takes each write a byte at a time, so
// two unsynchronised writers would interleave.
func TestBridge_ConcurrentWritesStayWellFormed(t *testing.T) {
	ps := &pollServer{tools: toolsV1, alternate: true, sseEvents: 300}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	s := startSession(t, srv.URL, time.Millisecond, true)
	s.handshake()
	s.send(`{"jsonrpc":"2.0","id":"long","method":"sse"}`)
	waitFor(t, "the streamed reply", func() bool { return strings.Contains(s.out.String(), `"id":"long"`) })
	s.close()
	lines := s.out.lines()
	if s.out.notifications() == 0 {
		t.Fatal("no notification was written while the stream ran; the test exercised nothing")
	}
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("stdout line %d is not JSON: %q", i, l)
		}
	}
}

// Review 20261003-195e F2 (design §18.14): the seed and the poll cover
// every tools/list page, not the first. The server answers one tool per
// page; the harness reads both pages; a change on the SECOND page notifies
// exactly once, and an unchanged multi-page list never does.
func TestBridgePoll_WalksEveryPage(t *testing.T) {
	ps := &pollServer{paged: true, tools: `[{"name":"list_my_setup"},{"name":"define_swarm","inputSchema":{"type":"object"}}]`}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	s := startSession(t, srv.URL, 5*time.Millisecond, false)
	s.handshake()
	s.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"cursor":"1"}}`)
	waitFor(t, "the harness's second page", func() bool { return strings.Contains(s.out.String(), `"id":3`) })
	before := ps.listCount()
	waitFor(t, "three paged polls", func() bool { return ps.listCount() >= before+6 })
	if n := s.out.notifications(); n != 0 {
		t.Fatalf("an unchanged paged list notified %d times", n)
	}
	ps.setTools(`[{"name":"list_my_setup"},{"name":"define_swarm","inputSchema":{"type":"object","properties":{"model":{}}}}]`)
	waitFor(t, "a list_changed for page two", func() bool { return s.out.notifications() >= 1 })
	before = ps.listCount()
	waitFor(t, "three more paged polls", func() bool { return ps.listCount() >= before+6 })
	s.close()
	if n := s.out.notifications(); n != 1 {
		t.Fatalf("list_changed written %d times for one change on page two", n)
	}
}

// Review 20261003-195e F4: the negotiated protocol version is the first
// initialize result's; a later initialize-shaped message does not rewrite
// what the poll (and the relay) send.
func TestBridgePoll_ProtocolVersionSetOnce(t *testing.T) {
	ps := &pollServer{tools: toolsV1, initVersions: []string{"2025-06-18", "1999-01-01"}}
	srv := httptest.NewServer(ps.handler())
	t.Cleanup(srv.Close)
	s := startSession(t, srv.URL, 5*time.Millisecond, false)
	s.handshake()
	s.send(`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{}}`)
	waitFor(t, "the second initialize reply", func() bool { return strings.Contains(s.out.String(), `"id":4`) })
	before := ps.listCount()
	waitFor(t, "two polls after it", func() bool { return ps.listCount() >= before+2 })
	s.close()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for i, v := range ps.versions[1:] {
		if v != "2025-06-18" {
			t.Fatalf("poll %d sent MCP-Protocol-Version %q", i+1, v)
		}
	}
}

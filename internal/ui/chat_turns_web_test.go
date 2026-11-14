package ui

// Web chat, the familiar chat UI (web-chat-design.md §2, GREEN
// review-20261114-c0e8). Operator, 2026-11-14: the Ask page's Send "doesn't
// seem to work" (the page waited about three minutes for the whole turn) and
// the reply was "super ugly, raw text, outside of chat boundaries" (the page
// rendered the model's whole transcript, tool results included).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

// parkedDispatcher holds a turn open until release is closed (or the turn's
// context ends), so a test can act while the turn runs.
type parkedDispatcher struct {
	started chan context.Context
	release chan struct{}
	reply   string
	panics  bool
}

func newParked(reply string) *parkedDispatcher {
	return &parkedDispatcher{started: make(chan context.Context, 8), release: make(chan struct{}), reply: reply}
}

func (p *parkedDispatcher) Process(ctx context.Context, req dispatcher.Request) dispatcher.Result {
	p.started <- ctx
	if p.panics {
		panic("dispatcher bug")
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return dispatcher.Result{Err: fmt.Errorf("LLM call failed: %w", ctx.Err())}
	}
	return dispatcher.Result{
		Text:     p.reply,
		Messages: append(req.Messages, chat.Message{Role: "assistant", Content: p.reply}),
	}
}

func (p *parkedDispatcher) ProcessStreaming(ctx context.Context, req dispatcher.Request, _ chat.StreamCallback) dispatcher.Result {
	return p.Process(ctx, req)
}

func (p *parkedDispatcher) waitStarted(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-p.started:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the dispatcher")
		return nil
	}
}

func turnServer(t *testing.T, d dispatcher.Doer) *Server {
	t.Helper()
	server, _ := chatTestServer(t, "")
	server.chatDispatcher = d
	return server
}

type ctxKey18 struct{}

func turnRequest(method, target, cookie string, form url.Values) *http.Request {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, body)
	req = asOperator491(req)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: cookie})
	return req
}

func startTurn(t *testing.T, s *Server, cookie, prompt string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ChatTurnStart(rec, turnRequest(http.MethodPost, "/projects/p1/chat/turns", cookie, url.Values{"prompt": {prompt}}), "p1")
	var out struct {
		Turn  string `json:"turn"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Turn != "" {
		return rec.Code, out.Turn
	}
	return rec.Code, out.Error.Code
}

type polled struct {
	State  string `json:"state"`
	Status string `json:"status"`
	HTML   string `json:"html"`
	Error  string `json:"error"`
}

func pollTurn(t *testing.T, s *Server, cookie, project, turn string) (int, polled) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ChatTurnPoll(rec, turnRequest(http.MethodGet, "/projects/"+project+"/chat/turns/"+turn, cookie, nil), project, turn)
	var p polled
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p
}

func pollUntil(t *testing.T, s *Server, turn, state string) polled {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, p := pollTurn(t, s, "c1", "p1", turn)
		if p.State == state {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("turn %s never reached %s", turn, state)
	return polled{}
}

// The page renders exchanges, not the model's transcript: no tool results,
// no role labels, one steps line with the tools' friendly phrases.
func TestChatView_RendersExchangesNotTheTranscript_WebChat(t *testing.T) {
	s := turnServer(t, newParked(""))
	key := chatHistoryKey("c1", true)
	history := []chat.Message{
		{Role: "user", Content: "What's in the news?"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{Function: chat.FunctionCall{Name: "memory_search"}}}},
		{Role: "tool", Content: "RAW-TOOL-RESULT <untrusted_content>"},
		{Role: "assistant", Content: "Let me check what's freshest.", ToolCalls: []chat.ToolCall{{Function: chat.FunctionCall{Name: "memory_search"}}, {Function: chat.FunctionCall{Name: "list_tasks"}}}},
		{Role: "tool", Content: "RAW-TOOL-RESULT two"},
		{Role: "assistant", Content: "## Today\n\n- Microsoft outage restored\n- " + strings.Repeat("x", 300)},
	}
	require.NoError(t, s.chatStoreFor("p1").Append(context.Background(), conversation.ChannelMessage{SessionID: key}, dispatcher.Result{Messages: history}))

	rec := httptest.NewRecorder()
	s.ChatPage(rec, turnRequest(http.MethodGet, "/projects/p1/chat", "c1", nil), "p1")
	body := rec.Body.String()
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, body, "RAW-TOOL-RESULT", "tool results are never rendered")
	assert.Equal(t, 1, strings.Count(body, `data-role="user"`))
	assert.Equal(t, 1, strings.Count(body, `data-role="assistant"`))
	assert.Contains(t, body, "searching memory ×2")
	assert.Contains(t, body, "checking your tasks")
	assert.Contains(t, body, "Let me check what&#39;s freshest.", "interim sentences sit in the steps details")
	assert.Contains(t, body, "<h2>Today</h2>", "the reply renders as markdown")
	assert.Contains(t, body, "<li>Microsoft outage restored</li>")
	assert.Contains(t, body, "chat-md", "the reply sits in the wrapping markdown container")
	assert.NotContains(t, body, ">tool<")
	assert.NotContains(t, body, "uppercase tracking-wide text-gray-400 mb-1\">assistant", "no role labels")
}

func TestChatView_ExchangesFromHistory_WebChat(t *testing.T) {
	ex := buildChatExchanges([]chat.Message{
		{Role: "assistant", Content: "AI notice"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{Function: chat.FunctionCall{Name: "list_tasks"}}}},
		{Role: "tool", Content: "x"},
		{Role: "assistant", Content: "hello"},
	}, map[int]string{4: "mark-1"})
	require.Len(t, ex, 2)
	assert.Equal(t, "", ex[0].User)
	assert.Equal(t, "AI notice", ex[0].Reply)
	assert.Equal(t, "hi", ex[1].User)
	assert.Equal(t, "checking your tasks", ex[1].Steps)
	assert.Equal(t, "hello", ex[1].Reply)
	assert.Equal(t, "mark-1", ex[1].Mark)
}

func TestChatMarkdown_RendersBlocksAndStaysInert_WebChat(t *testing.T) {
	out := string(renderChatMarkdown("# Title\n\n- a\n- b\n\n| h | i |\n|---|---|\n| 1 | 2 |\n\n```go\nx := 1\n```\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[click](javascript:alert(1)) [ok](https://example.com)"))
	for _, want := range []string{"<h1>Title</h1>", "<li>a</li>", "<table>", "<td>1</td>", "<pre><code", "x := 1", `href="https://example.com"`} {
		assert.Contains(t, out, want)
	}
	for _, bad := range []string{"<script", "<img", "onerror=", "javascript:"} {
		assert.NotContains(t, out, bad)
	}
}

// Send returns at once; the turn runs on; a second send for the same
// history is refused while it runs.
func TestChatTurn_StartReturnsBeforeTheTurnEnds_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	code, turn := startTurn(t, s, "c1", "hello")
	require.Equal(t, http.StatusAccepted, code)
	require.Len(t, turn, 32)
	d.waitStarted(t)

	code, errCode := startTurn(t, s, "c1", "again")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "TURN_RUNNING", errCode)
	close(d.release)
	pollUntil(t, s, turn, "done")
}

func TestChatTurn_ConcurrentStartsGiveOneTurn_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	var accepted, conflicts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch code, _ := startTurn(t, s, "c1", "hi"); code {
			case http.StatusAccepted:
				accepted.Add(1)
			case http.StatusConflict:
				conflicts.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), accepted.Load())
	assert.Equal(t, int32(7), conflicts.Load())
	close(d.release)
}

// The turn outlives its request: the request context's values reach the
// dispatcher, its cancellation does not.
func TestChatTurn_OutlivesItsRequest_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	reqCtx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey18{}, "principal"))
	req := turnRequest(http.MethodPost, "/projects/p1/chat/turns", "c1", url.Values{"prompt": {"hi"}})
	req = req.WithContext(context.WithValue(asOperator491(req.WithContext(reqCtx)).Context(), ctxKey18{}, "principal"))
	rec := httptest.NewRecorder()
	s.ChatTurnStart(rec, req, "p1")
	require.Equal(t, http.StatusAccepted, rec.Code)
	turnCtx := d.waitStarted(t)
	cancel()
	time.Sleep(20 * time.Millisecond)
	assert.NoError(t, turnCtx.Err(), "the request ending must not end the turn")
	assert.Equal(t, "principal", turnCtx.Value(ctxKey18{}), "the request's values reach the turn")
	close(d.release)
}

func TestChatTurn_PollShowsStatusThenDone_WebChat(t *testing.T) {
	d := newParked("**Hi** there")
	s := turnServer(t, d)
	_, turn := startTurn(t, s, "c1", "hello")
	d.waitStarted(t)
	key := chatHistoryKey("c1", true)

	s.SetWebChatTurnStatus(key, "⏳ searching memory · 3s · so far: searching memory")
	code, p := pollTurn(t, s, "c1", "p1", turn)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "running", p.State)
	assert.Equal(t, "searching memory · 3s · so far: searching memory", p.Status)

	s.SetWebChatTurnStatus(key, "⏳ checking your tasks · 5s · so far: searching memory, checking your tasks")
	_, p = pollTurn(t, s, "c1", "p1", turn)
	assert.Contains(t, p.Status, "checking your tasks")

	close(d.release)
	p = pollUntil(t, s, turn, "done")
	assert.Contains(t, p.HTML, "<strong>Hi</strong> there")
	assert.Contains(t, p.HTML, "hello", "the exchange carries the user's message")

	// A line after the turn ended is dropped; it never reaches the next turn.
	s.SetWebChatTurnStatus(key, "⏳ late line")
	d2 := newParked("second")
	s.chatDispatcher = d2
	_, turn2 := startTurn(t, s, "c1", "again")
	d2.waitStarted(t)
	_, p = pollTurn(t, s, "c1", "p1", turn2)
	assert.NotContains(t, p.Status, "late line")
	close(d2.release)
}

func TestChatTurn_UnknownAndExpiredAre404_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	code, _ := pollTurn(t, s, "c1", "p1", strings.Repeat("a", 32))
	assert.Equal(t, http.StatusNotFound, code)

	_, turn := startTurn(t, s, "c1", "hello")
	d.waitStarted(t)
	close(d.release)
	pollUntil(t, s, turn, "done")
	reg := s.chatTurnRegistry()
	reg.mu.Lock()
	reg.now = func() time.Time { return time.Now().Add(chatTurnKeep + time.Minute) }
	reg.mu.Unlock()
	code, _ = pollTurn(t, s, "c1", "p1", turn)
	assert.Equal(t, http.StatusNotFound, code)
}

// Stop ends the turn and leaves the stored history as it was.
func TestChatTurn_StopKeepsTheHistory_WebChat(t *testing.T) {
	d := newParked("never")
	s := turnServer(t, d)
	key := chatHistoryKey("c1", true)
	before := []chat.Message{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "earlier reply"}}
	require.NoError(t, s.chatStoreFor("p1").Append(context.Background(), conversation.ChannelMessage{SessionID: key}, dispatcher.Result{Messages: before}))

	_, turn := startTurn(t, s, "c1", "stop me")
	turnCtx := d.waitStarted(t)
	rec := httptest.NewRecorder()
	s.ChatTurnCancel(rec, turnRequest(http.MethodPost, "/projects/p1/chat/turns/"+turn+"/cancel", "c1", url.Values{}), "p1", turn)
	require.Equal(t, http.StatusOK, rec.Code)
	pollUntil(t, s, turn, "cancelled")
	assert.Error(t, turnCtx.Err())
	assert.Equal(t, before, s.chatStoreFor("p1").History(key), "a stopped turn stores nothing")

	page := httptest.NewRecorder()
	s.ChatPage(page, turnRequest(http.MethodGet, "/projects/p1/chat", "c1", nil), "p1")
	assert.NotContains(t, page.Body.String(), "stop me")
}

func TestChatTurn_CancelAfterDoneStaysDone_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	_, turn := startTurn(t, s, "c1", "hi")
	d.waitStarted(t)
	close(d.release)
	pollUntil(t, s, turn, "done")
	rec := httptest.NewRecorder()
	s.ChatTurnCancel(rec, turnRequest(http.MethodPost, "/projects/p1/chat/turns/"+turn+"/cancel", "c1", url.Values{}), "p1", turn)
	_, p := pollTurn(t, s, "c1", "p1", turn)
	assert.Equal(t, "done", p.State)
}

func TestChatTurn_TimeoutFails_WebChat(t *testing.T) {
	d := newParked("never")
	s := turnServer(t, d)
	s.chatTurnTimeout = 30 * time.Millisecond
	_, turn := startTurn(t, s, "c1", "slow")
	p := pollUntil(t, s, turn, "failed")
	assert.Contains(t, p.Error, "did not answer")
}

func TestChatTurn_PanicMarksFailed_WebChat(t *testing.T) {
	d := newParked("")
	d.panics = true
	var held atomic.Int32
	s := turnServer(t, d)
	s.longWork = func() (func(), bool) { held.Add(1); return func() { held.Add(-1) }, true }
	_, turn := startTurn(t, s, "c1", "boom")
	p := pollUntil(t, s, turn, "failed")
	assert.Equal(t, "Something went wrong.", p.Error)
	assert.Equal(t, int32(0), held.Load(), "the gate count is released")
}

// A turn belongs to one browser, one view and one project.
func TestChatTurn_OthersGet404_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	_, turn := startTurn(t, s, "c1", "mine")
	d.waitStarted(t)
	defer close(d.release)

	code, _ := pollTurn(t, s, "other-cookie", "p1", turn)
	assert.Equal(t, http.StatusNotFound, code, "another browser")
	code, _ = pollTurn(t, s, "c1", "p2", turn)
	assert.Equal(t, http.StatusNotFound, code, "another project")

	// The member view of the same cookie is a different history.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/chat/turns/"+turn, nil)
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "c1"})
	s.ChatTurnPoll(rec, req, "p1", turn)
	assert.NotEqual(t, http.StatusOK, rec.Code, "the other view never sees the turn")

	rec = httptest.NewRecorder()
	s.ChatTurnCancel(rec, turnRequest(http.MethodPost, "/projects/p1/chat/turns/"+turn+"/cancel", "other-cookie", url.Values{}), "p1", turn)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestChatTurn_UnscopedNonAdminIs403_WebChat(t *testing.T) {
	c := newUIChain466With(t, true, &countingExplainer491{})
	code, body := c.serve(t, http.MethodPost, "/ui/projects/alpha/chat/turns", staticKey491UI)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "PROJECT_SCOPE_REQUIRED")
}

// The deploy gate counts a running turn, and a draining daemon refuses one.
func TestChatTurn_DeployGate_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	var held atomic.Int32
	draining := false
	s.longWork = func() (func(), bool) {
		if draining {
			return nil, false
		}
		held.Add(1)
		return func() { held.Add(-1) }, true
	}
	_, turn := startTurn(t, s, "c1", "hi")
	d.waitStarted(t)
	assert.Equal(t, int32(1), held.Load(), "a running turn is counted")
	close(d.release)
	pollUntil(t, s, turn, "done")
	assert.Eventually(t, func() bool { return held.Load() == 0 }, 2*time.Second, 5*time.Millisecond)

	draining = true
	code, errCode := startTurn(t, s, "c1", "hi")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "DRAINING", errCode)
}

func TestChatTurn_BusyBeyondTheCap_WebChat(t *testing.T) {
	old := chatTurnsMax
	chatTurnsMax = 2
	defer func() { chatTurnsMax = old }()
	d := newParked("done")
	s := turnServer(t, d)
	for _, c := range []string{"a", "b"} {
		code, _ := startTurn(t, s, c, "hi")
		require.Equal(t, http.StatusAccepted, code)
	}
	code, errCode := startTurn(t, s, "c", "hi")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "TURNS_BUSY", errCode)
	close(d.release)
}

func TestChatReset_RefusedWhileATurnRuns_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	key := chatHistoryKey("c1", true)
	require.NoError(t, s.chatStoreFor("p1").Append(context.Background(), conversation.ChannelMessage{SessionID: key}, dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "keep"}}}))
	_, turn := startTurn(t, s, "c1", "hi")
	d.waitStarted(t)

	rec := httptest.NewRecorder()
	s.ChatReset(rec, turnRequest(http.MethodPost, "/projects/p1/chat/reset", "c1", url.Values{}), "p1")
	assert.Equal(t, http.StatusConflict, rec.Code)
	close(d.release)
	pollUntil(t, s, turn, "done")

	rec = httptest.NewRecorder()
	s.ChatReset(rec, turnRequest(http.MethodPost, "/projects/p1/chat/reset", "c1", url.Values{}), "p1")
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Empty(t, s.chatStoreFor("p1").History(key))
}

// A reload during a turn shows the pending message and resumes polling.
func TestChatPage_ReloadDuringATurn_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	_, turn := startTurn(t, s, "c1", "pending question")
	d.waitStarted(t)
	rec := httptest.NewRecorder()
	s.ChatPage(rec, turnRequest(http.MethodGet, "/projects/p1/chat", "c1", nil), "p1")
	body := rec.Body.String()
	assert.Contains(t, body, "pending question")
	assert.Contains(t, body, `data-turn="`+turn+`"`)
	close(d.release)
}

// Without JavaScript the form posts as before; the reply comes from the same
// registry turn, counted by the gate.
func TestChatPostMessage_NoJSUsesTheRegistry_WebChat(t *testing.T) {
	s, _ := chatTestServer(t, "plain reply")
	var counted atomic.Int32
	s.longWork = func() (func(), bool) { counted.Add(1); return func() {}, true }
	rec := httptest.NewRecorder()
	s.ChatPostMessage(rec, turnRequest(http.MethodPost, "/projects/p1/chat/messages", "c1", url.Values{"prompt": {"hi"}}), "p1")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "plain reply")
	assert.Equal(t, int32(1), counted.Load())
}

func TestChatStatusSink_ReachesOnlyItsTurn_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	_, t1 := startTurn(t, s, "c1", "one")
	_, t2 := startTurn(t, s, "c2", "two")
	d.waitStarted(t)
	d.waitStarted(t)
	s.SetWebChatTurnStatus(chatHistoryKey("c1", true), "⏳ for c1")
	_, p1 := pollTurn(t, s, "c1", "p1", t1)
	_, p2 := pollTurn(t, s, "c2", "p1", t2)
	assert.Equal(t, "for c1", p1.Status)
	assert.NotContains(t, p2.Status, "c1")
	close(d.release)
}

func TestChatPage_LoadsTheChatClient_WebChat(t *testing.T) {
	s := turnServer(t, newParked(""))
	rec := httptest.NewRecorder()
	s.ChatPage(rec, turnRequest(http.MethodGet, "/projects/p1/chat", "c1", nil), "p1")
	body := rec.Body.String()
	assert.Contains(t, body, `src="/ui/static/chat.js?v=`)
	assert.Contains(t, body, `data-chat-turns="/ui/projects/p1/chat/turns"`)
	assert.Contains(t, body, `data-testid="chat-composer"`)
	assert.Contains(t, body, `data-testid="chat-send-button"`)
}

// A running turn in one project does not block a new chat in another: the
// turn writes only to its own project's store (review-20261114-3435 F2).
func TestChatReset_OtherProjectIsAllowed_WebChat(t *testing.T) {
	d := newParked("done")
	s := turnServer(t, d)
	_, turn := startTurn(t, s, "c1", "hi")
	d.waitStarted(t)
	key := chatHistoryKey("c1", true)
	_, inP1 := s.chatTurnRegistry().runningFor(key, "p1")
	_, inP2 := s.chatTurnRegistry().runningFor(key, "p2")
	assert.True(t, inP1, "the turn runs in p1")
	assert.False(t, inP2, "so a reset in p2 is not refused")
	close(d.release)
	pollUntil(t, s, turn, "done")
}

// The no-JS page says "Stopped." when its turn was stopped (F5).
func TestChatPostMessage_NoJSStopped_WebChat(t *testing.T) {
	d := newParked("never")
	s := turnServer(t, d)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.ChatPostMessage(rec, turnRequest(http.MethodPost, "/projects/p1/chat/messages", "c1", url.Values{"prompt": {"hi"}}), "p1")
		close(done)
	}()
	d.waitStarted(t)
	v, ok := s.chatTurnRegistry().runningFor(chatHistoryKey("c1", true), "p1")
	require.True(t, ok)
	require.True(t, s.chatTurnRegistry().stop(v.ID, chatHistoryKey("c1", true), "p1"))
	<-done
	assert.Contains(t, rec.Body.String(), "Stopped.")
}

func TestChatTurn_DisabledHasItsOwnCode_WebChat(t *testing.T) {
	s := turnServer(t, newParked(""))
	s.chatDispatcher = nil
	code, errCode := startTurn(t, s, "c1", "hi")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "DISABLED", errCode)
}

// A web chat turn tells the dispatcher it cannot report back later, so it
// does not promise to (web-chat-design.md §4; operator, 2026-11-14).
func TestChatTurn_TellsTheDispatcherItCannotReportBack_WebChat(t *testing.T) {
	s, stub := chatTestServer(t, "ok")
	rec := httptest.NewRecorder()
	s.ChatPostMessage(rec, turnRequest(http.MethodPost, "/projects/p1/chat/messages", "c1", url.Values{"prompt": {"refresh the news"}}), "p1")
	require.Equal(t, http.StatusOK, rec.Code)
	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.Len(t, stub.captured, 1)
	assert.True(t, stub.captured[0].NoLaterDelivery)
}

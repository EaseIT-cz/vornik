package ui

// Web chat part 2: the reply streams in (web-chat-design.md §3, GREEN
// review-20261114-5cf7). Operator, 2026-11-14: "then do the streaming part 2".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
)

// streamingDispatcher streams its accumulated texts, then parks until
// released (or the turn's context ends). On release it answers with an
// interim sentence, a tool call and a reply, as the real dispatcher stores.
type streamingDispatcher struct {
	texts     []string // accumulated texts handed to onText, in order
	started   chan context.Context
	release   chan struct{}
	fail      bool // on release, end with Result.Err instead of a reply
	streamed  atomic.Int32
	processed atomic.Int32
}

func newStreaming(texts ...string) *streamingDispatcher {
	return &streamingDispatcher{texts: texts, started: make(chan context.Context, 4), release: make(chan struct{})}
}

func (d *streamingDispatcher) result(req dispatcher.Request) dispatcher.Result {
	msgs := append(append([]chat.Message(nil), req.Messages...),
		chat.Message{Role: "assistant", Content: "Let me check.", ToolCalls: []chat.ToolCall{{Function: chat.FunctionCall{Name: "list_tasks"}}}},
		chat.Message{Role: "tool", Content: "RAW"},
		chat.Message{Role: "assistant", Content: "Hello **there**"},
	)
	return dispatcher.Result{Text: "Hello **there**", Messages: msgs}
}

func (d *streamingDispatcher) Process(ctx context.Context, req dispatcher.Request) dispatcher.Result {
	d.processed.Add(1)
	d.started <- ctx
	<-d.release
	return d.result(req)
}

func (d *streamingDispatcher) ProcessStreaming(ctx context.Context, req dispatcher.Request, onText chat.StreamCallback) dispatcher.Result {
	d.streamed.Add(1)
	for _, t := range d.texts {
		onText(t)
	}
	d.started <- ctx
	select {
	case <-d.release:
	case <-ctx.Done():
		return dispatcher.Result{Err: fmt.Errorf("LLM call failed: %w", ctx.Err())}
	}
	if d.fail {
		return dispatcher.Result{Err: errors.New("LLM call failed: upstream 500")}
	}
	return d.result(req)
}

func (d *streamingDispatcher) waitStarted(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-d.started:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the dispatcher")
		return nil
	}
}

type polledFull struct {
	State, Status, HTML, Error string
	Header                     http.Header
}

func pollFull(t *testing.T, s *Server, turn string) polledFull {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ChatTurnPoll(rec, turnRequest(http.MethodGet, "/projects/p1/chat/turns/"+turn, "c1", nil), "p1", turn)
	var p struct{ State, Status, HTML, Error string }
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return polledFull{State: p.State, Status: p.Status, HTML: p.HTML, Error: p.Error, Header: rec.Header()}
}

func pollFullUntil(t *testing.T, s *Server, turn, state string) polledFull {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p := pollFull(t, s, turn); p.State == state {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("turn never reached %s", state)
	return polledFull{}
}

// While the model writes, the poll carries the partial reply rendered on the
// server, and model markup in it stays inert.
func TestChatStream_PartialRendersAsItGrows_WebChat(t *testing.T) {
	d := newStreaming("Hel", "Hello **there** <script>alert(1)</script> <img src=x onerror=alert(1)>")
	s := turnServer(t, d)
	_, turn := startTurn(t, s, "c1", "hi")
	d.waitStarted(t)
	p := pollFull(t, s, turn)
	require.Equal(t, "running", p.State)
	assert.Contains(t, p.HTML, "<strong>there</strong>")
	assert.NotContains(t, p.HTML, "<script")
	assert.NotContains(t, p.HTML, "<img")
	assert.Equal(t, int32(1), d.streamed.Load(), "the script's turn streams")
	close(d.release)
	done := pollFullUntil(t, s, turn, "done")
	assert.Contains(t, done.HTML, "<strong>there</strong>")
}

// Past the cap the partial keeps its head and its last 4 KiB, so a notice
// appended last still shows.
func TestChatStream_CapKeepsHeadAndTail_WebChat(t *testing.T) {
	reg := newChatTurnRegistry()
	tr, err := reg.start("k", "p1", "hi", func() {})
	require.NoError(t, err)
	reg.appendPartial(tr, "HEAD")
	reg.appendPartial(tr, strings.Repeat("x", chatPartialHead+10_000))
	reg.appendPartial(tr, "\n\n[Stopped.]")
	text, _ := reg.partialOf(tr)
	assert.True(t, strings.HasPrefix(text, "HEAD"))
	assert.True(t, strings.HasSuffix(text, "[Stopped.]"))
	assert.LessOrEqual(t, len(text), chatPartialHead+chatPartialTail+16)

	reg.finish(tr, chatTurnDone, nil)
	reg.appendPartial(tr, "late")
	after, _ := reg.partialOf(tr)
	assert.Equal(t, text, after, "a delta after the turn ended is dropped")
}

// Every delivered text is marked when the turn ends: the interim sentence
// (streaming delivers it) and the reply, each with its own mark.
func TestChatStream_MarksEveryDeliveredText_WebChat(t *testing.T) {
	d := newStreaming("Let me check.", "Let me check.\n\nHello **there**")
	s := turnServer(t, d)
	marker, _, ledger := uiTestMarker()
	s.aiMarker = marker
	_, turn := startTurn(t, s, "c1", "hi")
	d.waitStarted(t)
	close(d.release)
	p := pollFullUntil(t, s, turn, "done")
	assert.Len(t, dataMarkRE.FindAllStringSubmatch(p.HTML, -1), 2, "interim sentence and reply")
	assert.Equal(t, "ai-generated", p.Header.Get("AI-Disclosure"))
	assert.NotEmpty(t, p.Header.Get("X-Vornik-AI-Mark"))
	ledger.mu.Lock()
	assert.Len(t, ledger.rows, 2)
	ledger.mu.Unlock()
}

// Stop keeps what was written: the receiver ends the stream with its own
// "[Stopped.]" because the cancel cause is ErrStoppedByUser; the partial is
// marked and not stored.
func TestChatStream_StopKeepsThePartial_WebChat(t *testing.T) {
	d := newStreaming("Hel", "Hello")
	s := turnServer(t, d)
	marker, _, _ := uiTestMarker()
	s.aiMarker = marker
	key := chatHistoryKey("c1", true)
	_, turn := startTurn(t, s, "c1", "hi")
	turnCtx := d.waitStarted(t)
	rec := httptest.NewRecorder()
	s.ChatTurnCancel(rec, turnRequest(http.MethodPost, "/projects/p1/chat/turns/"+turn+"/cancel", "c1", url.Values{}), "p1", turn)
	p := pollFullUntil(t, s, turn, "cancelled")
	assert.ErrorIs(t, context.Cause(turnCtx), dispatcher.ErrStoppedByUser)
	assert.Contains(t, p.HTML, "Hello")
	assert.Contains(t, p.HTML, "[Stopped.]")
	assert.Len(t, dataMarkRE.FindAllStringSubmatch(p.HTML, -1), 1, "the delivered partial is marked")
	assert.Empty(t, s.chatStoreFor("p1").History(key), "a stopped turn stores nothing")
}

// A failed streamed turn keeps its text, which ends with the receiver's notice.
func TestChatStream_FailedKeepsThePartial_WebChat(t *testing.T) {
	d := newStreaming("Half an ans")
	d.fail = true
	s := turnServer(t, d)
	_, turn := startTurn(t, s, "c1", "hi")
	d.waitStarted(t)
	close(d.release)
	p := pollFullUntil(t, s, turn, "failed")
	assert.Contains(t, p.HTML, "Half an ans")
	assert.Contains(t, p.HTML, "This reply was cut off")
}

// The no-JS send keeps the one-shot channel: nobody polls its turn.
func TestChatStream_NoJSDoesNotStream_WebChat(t *testing.T) {
	d := newStreaming("x")
	s := turnServer(t, d)
	rec := httptest.NewRecorder()
	go func() {
		d.waitStarted(t)
		close(d.release)
	}()
	s.ChatPostMessage(rec, turnRequest(http.MethodPost, "/projects/p1/chat/messages", "c1", url.Values{"prompt": {"hi"}}), "p1")
	assert.Equal(t, int32(0), d.streamed.Load())
	assert.Equal(t, int32(1), d.processed.Load())
	assert.Contains(t, rec.Body.String(), "<strong>there</strong>")
}

// Streaming and polling together, for the race detector.
func TestChatStream_PollWhileStreaming_WebChat(t *testing.T) {
	d := newStreaming()
	for i := 1; i <= 50; i++ {
		d.texts = append(d.texts, strings.Repeat("w ", i))
	}
	s := turnServer(t, d)
	_, turn := startTurn(t, s, "c1", "hi")
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				pollFull(t, s, turn)
			}
		}
	}()
	d.waitStarted(t)
	close(d.release)
	pollFullUntil(t, s, turn, "done")
	close(stop)
}

// ctxLedger refuses a record on a finished context, as a database ledger
// does (review-20261114-bb1a F1: the stopped partial was marked on the
// turn's cancelled context, so its ledger row was lost).
type ctxLedger struct{ uiMarkLedger }

func (l *ctxLedger) Record(ctx context.Context, m persistence.AIOutputMark) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.uiMarkLedger.Record(ctx, m)
}

func TestChatStream_StoppedPartialIsRecorded_WebChat(t *testing.T) {
	d := newStreaming("Hel", "Hello")
	s := turnServer(t, d)
	signer := aimark.NewSigner(make([]byte, 32))
	ledger := &ctxLedger{}
	s.aiMarker = aimark.NewMarker(signer, ledger, func() string { return "test" }, nil, nil)
	_, turn := startTurn(t, s, "c1", "hi")
	d.waitStarted(t)
	rec := httptest.NewRecorder()
	s.ChatTurnCancel(rec, turnRequest(http.MethodPost, "/projects/p1/chat/turns/"+turn+"/cancel", "c1", url.Values{}), "p1", turn)
	pollFullUntil(t, s, turn, "cancelled")
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	assert.Len(t, ledger.rows, 1, "the stopped partial's mark has its ledger row")
}

// A multibyte character split across two deltas stays whole (F2).
func TestChatStream_SplitRuneStaysWhole_WebChat(t *testing.T) {
	reg := newChatTurnRegistry()
	tr, err := reg.start("k", "p1", "hi", func() {})
	require.NoError(t, err)
	euro := "€" // three bytes
	reg.appendPartial(tr, "price "+euro[:1])
	mid, _ := reg.partialOf(tr)
	assert.True(t, utf8.ValidString(mid), "an unfinished rune is held back")
	reg.appendPartial(tr, euro[1:]+"5")
	text, _ := reg.partialOf(tr)
	assert.Equal(t, "price €5", text)
}

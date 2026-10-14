package github

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// GitHub #142 item #22 (outcome-quality LLD §S6 #22). A GitHub @vornik mention
// turn ran synchronously inside the webhook handler on r.Context(). GitHub
// stops waiting for a delivery after about 10 seconds and closes the
// connection; net/http then cancels r.Context(), so a turn that takes longer
// than that was cancelled mid-way and the thread got no reply.

// blockingReceiver holds every Receive until release is closed, and records the
// context each call ran on.
type blockingReceiver struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int64

	mu   sync.Mutex
	ctxs []context.Context
}

func newBlockingReceiver() *blockingReceiver {
	return &blockingReceiver{started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (b *blockingReceiver) Receive(ctx context.Context, _ conversation.ChannelMessage) error {
	b.calls.Add(1)
	b.mu.Lock()
	b.ctxs = append(b.ctxs, ctx)
	b.mu.Unlock()
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

func (b *blockingReceiver) ctx(i int) context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctxs[i]
}

func mentionBody() []byte {
	return []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 3, "title": "q"},
		"comment": {"id": 77, "body": "@vornik-companion how is it going?", "user": {"login": "vadim"}}
	}`)
}

func bindReceiver(ch *Channel, rx conversation.Receiver) {
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()
}

// audit 2026-10-12 (#142 #22): the delivery is acknowledged before the turn
// runs. Against a real HTTP server: the 200 must arrive while the turn is still
// blocked. Before the fix the response waited for the whole turn.
func TestHandleWebhook_MentionIsAcknowledgedBeforeTheTurnRuns(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	rx := newBlockingReceiver()
	bindReceiver(ch, rx)
	srv := httptest.NewServer(http.HandlerFunc(ch.HandleWebhook))
	defer srv.Close()
	defer close(rx.release)

	req := signedRequest("shhh", "issue_comment", "d-ack", mentionBody())
	out, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/github-app/webhook", bytes.NewReader(mentionBody()))
	if err != nil {
		t.Fatal(err)
	}
	out.Header = req.Header.Clone()

	got := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(out)
		if err != nil {
			got <- -1
			return
		}
		_ = resp.Body.Close()
		got <- resp.StatusCode
	}()

	<-rx.started
	select {
	case code := <-got:
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook was not acknowledged while the mention turn was running: " +
			"GitHub gives up on a delivery after ~10s and the turn rides on its connection")
	}
}

// audit 2026-10-12 (#142 #22): a dropped webhook connection must not cancel the
// turn. net/http cancels r.Context() when the client goes away; the turn must
// not run on that context. Cancellation propagates synchronously to derived
// contexts, so checking Err() right after the cancel is deterministic.
func TestHandleWebhook_DroppedConnectionDoesNotCancelTheTurn(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	rx := newBlockingReceiver()
	bindReceiver(ch, rx)

	reqCtx, dropConnection := context.WithCancel(context.Background())
	r := signedRequest("shhh", "issue_comment", "d-drop", mentionBody()).WithContext(reqCtx)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		ch.HandleWebhook(httptest.NewRecorder(), r)
	}()

	<-rx.started
	dropConnection()
	if err := rx.ctx(0).Err(); err != nil {
		close(rx.release)
		<-handlerDone
		t.Fatalf("the mention turn was cancelled when the webhook connection dropped: %v", err)
	}
	close(rx.release)
	<-handlerDone
	ch.waitInFlight()
}

// audit 2026-10-12 (#142 #22): a redelivery of the same X-GitHub-Delivery runs
// no second turn. An operator who redelivers a delivery GitHub marked as timed
// out (it was, until the ack moved first) must not get two replies.
func TestHandleWebhook_RedeliveredMentionRunsOnce(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	rx := &recordingReceiver{}
	bindReceiver(ch, rx)

	for range 2 {
		w := httptest.NewRecorder()
		ch.HandleWebhook(w, signedRequest("shhh", "issue_comment", "d-same", mentionBody()))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a duplicate is acknowledged, not refused)", w.Code)
		}
		ch.waitInFlight()
	}
	if n := len(rx.copyGot()); n != 1 {
		t.Fatalf("a redelivered mention ran %d turns, want 1", n)
	}

	// A different delivery is a different comment event and runs.
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-other", mentionBody()))
	ch.waitInFlight()
	if n := len(rx.copyGot()); n != 2 {
		t.Fatalf("a new delivery ran %d turns in total, want 2", n)
	}
}

// audit 2026-10-12 (#142 #22): stopping the channel cancels a running turn and
// waits for it, so a daemon shutdown neither leaks the goroutine nor waits out
// a ten-minute turn.
func TestChannelStop_CancelsAndDrainsARunningTurn(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	rx := newBlockingReceiver()
	bindReceiver(ch, rx)

	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-stop", mentionBody()))
	<-rx.started

	stopped := make(chan struct{})
	go func() {
		_ = ch.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		close(rx.release)
		t.Fatal("Stop did not cancel the running turn")
	}
	if rx.ctx(0).Err() == nil {
		t.Fatal("the turn's context is still live after Stop returned")
	}
}

// The turn context carries no deadline of its own, so the receiver's turn
// bound (dispatcher.ChannelReceiver.TurnTimeout, 10m, §S5 #5) applies and
// names itself. A deadline here would make the receiver skip its own bound.
func TestHandleWebhook_TurnContextLeavesTheDeadlineToTheReceiver(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	rx := newBlockingReceiver()
	bindReceiver(ch, rx)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-deadline", mentionBody()))
	<-rx.started
	if _, ok := rx.ctx(0).Deadline(); ok {
		t.Error("the detached turn context has a deadline; the receiver's TurnTimeout would not apply")
	}
	close(rx.release)
	ch.waitInFlight()
}

// replyThenFailReceiver posts its reply through the channel, then fails a
// post-reply step (as ChannelReceiver does when the session append fails).
type replyThenFailReceiver struct {
	ch    *Channel
	turns atomic.Int64
	reply bool
}

func (r *replyThenFailReceiver) Receive(ctx context.Context, m conversation.ChannelMessage) error {
	r.turns.Add(1)
	if r.reply {
		if _, err := r.ch.Send(ctx, conversation.ChannelMessage{SessionID: m.SessionID, Text: "the answer"}); err != nil {
			return err
		}
	}
	return errors.New("dispatcher: session append: database is locked")
}

// audit 2026-10-12 (#142 #22, review-20261013-b982 #2): a turn that already
// posted its reply and then failed a later step keeps its delivery id, so a
// redelivery does not post a second comment.
func TestHandleWebhook_RedeliveryAfterAPostedReplyDoesNotPostAgain(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)
	rx := &replyThenFailReceiver{ch: ch, reply: true}
	bindReceiver(ch, rx)
	body := []byte(`{"action":"created","repository":{"full_name":"acme/api"},"sender":{"login":"vadim"},"installation":{"id":99},"issue":{"number":3,"title":"q"},"comment":{"id":77,"body":"@vornik-companion hi","user":{"login":"vadim"}}}`)
	for range 2 {
		ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-replied", body))
		ch.waitInFlight()
	}
	if n := stub.commentCalls.Load(); n != 1 {
		t.Fatalf("comments posted = %d, want 1: the redelivery re-ran a turn that had already replied", n)
	}
}

// A turn that failed before replying is retried by a redelivery.
func TestHandleWebhook_RedeliveryAfterAFailedTurnRetries(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	rx := &replyThenFailReceiver{ch: ch}
	bindReceiver(ch, rx)
	for range 2 {
		ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-failed", mentionBody()))
		ch.waitInFlight()
	}
	if n := rx.turns.Load(); n != 2 {
		t.Fatalf("turns = %d, want 2: a redelivery of a failed turn must retry it", n)
	}
}

// With no receiver bound nothing runs and nothing is remembered, so a later
// delivery of the same id, once a receiver is bound, runs.
func TestHandleWebhook_UnboundDeliveryIsNotRemembered(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-unbound", mentionBody()))
	rx := &recordingReceiver{}
	bindReceiver(ch, rx)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-unbound", mentionBody()))
	ch.waitInFlight()
	if n := len(rx.copyGot()); n != 1 {
		t.Fatalf("turns = %d, want 1", n)
	}
}

// A Channel built as a struct literal (the service package does, for its
// orphan checks) must start and stop without the fields New initialises.
func TestZeroValueChannel_StartStopDoesNotPanic(t *testing.T) {
	c := &Channel{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx, &recordingReceiver{}) }()
	cancel()
	<-done
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if !c.acceptDelivery("d", time.Now()) {
		t.Fatal("a zero-value channel must accept a new delivery")
	}
}

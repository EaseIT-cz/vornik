package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// audit 2026-10-12 (#142 #20, outcome-quality LLD §S6 #20): a streamed reply
// cut off mid-stream ends the turn in an error (§S2 C3), but the partial text
// had already reached the live preview through onText. The streaming branch
// closed the stream without a word, so on Telegram the partial text stayed in
// the chat looking like a finished answer (or the "…" placeholder stayed
// forever). The stream now ends with a notice that says the reply failed.

func TestStreamingTurnFailure_PreviewEndsWithACutOffNotice(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{
		streamingTokens: []string{"The answer is", "The answer is forty"},
		streamingResult: Result{Err: fmt.Errorf("LLM call failed: %w", errors.New("connection reset"))},
	}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-cut", Text: "q"})
	if err == nil {
		t.Fatal("Receive must still return the turn's error")
	}
	st := ch.lastStream()
	got := strings.Join(st.deltas, "")
	if !strings.HasPrefix(got, "The answer is forty") {
		t.Fatalf("the streamed text changed: %q", got)
	}
	if !strings.Contains(got, "cut off") || !strings.Contains(got, "incomplete") {
		t.Fatalf("the preview ends with the partial text and no notice: %q", got)
	}
	if !st.closed {
		t.Error("the stream was not closed")
	}
	if n := len(ch.sentCopy()); n != 0 {
		t.Errorf("a separate message was sent (%d); the notice belongs on the stream", n)
	}
}

func TestStreamingTurnFailure_NothingStreamedStillGetsANotice(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{streamingResult: Result{Err: errors.New("LLM call failed: upstream 500")}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	_ = rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-none", Text: "q"})
	got := strings.Join(ch.lastStream().deltas, "")
	if !strings.Contains(got, "could not") {
		t.Fatalf("an empty preview (Telegram's placeholder) must be replaced by a failure notice, got %q", got)
	}
}

func TestStreamingTurnFailure_TurnDeadlineNamesTheTimeLimit(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	agent := &stubAgent{
		streamingTokens: []string{"partial"},
		streamingResult: Result{Err: fmt.Errorf("LLM call failed: %w", context.DeadlineExceeded)},
	}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}
	_ = rcv.Receive(ctx, conversation.ChannelMessage{SessionID: "s-late", Text: "q"})
	got := strings.Join(ch.lastStream().deltas, "")
	if !strings.Contains(got, "time limit") {
		t.Fatalf("a turn ended by its deadline must say so, got %q", got)
	}
}

// When the stream itself is broken, the notice goes out as one message instead,
// so the failure is not silent either way.
func TestStreamingTurnFailure_BrokenStreamSendsTheNoticeSeparately(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{
		streamingTokens: []string{"partial"},
		streamingResult: Result{Err: errors.New("LLM call failed: boom")},
	}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}
	// The first Append succeeds; break the stream for the notice.
	agent.onAfterTokens = func() { ch.lastStream().setAppendErr(errors.New("edit refused")) }
	_ = rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-broken", Text: "q"})
	sent := ch.sentCopy()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "cut off") {
		t.Fatalf("sent = %+v, want one cut-off notice", sent)
	}
}

// A successful turn is unchanged: no notice.
func TestStreamingTurnSuccess_HasNoNotice(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{streamingTokens: []string{"ok"}, streamingResult: Result{Text: "ok"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}
	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-ok", Text: "q"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ch.lastStream().deltas, ""); got != "ok" {
		t.Fatalf("deltas = %q, want %q", got, "ok")
	}
}

// review-20261013-383f #1: the notice was appended (buffered) but the closing
// edit that would have shown it failed. It must then go out as one message.
func TestStreamingTurnFailure_FailedCloseSendsTheNoticeSeparately(t *testing.T) {
	ch := &stubStreamingChannel{stubChannel: stubChannel{name: "stub"}}
	agent := &stubAgent{
		streamingTokens: []string{"partial"},
		streamingResult: Result{Err: errors.New("LLM call failed: boom")},
	}
	agent.onAfterTokens = func() { ch.lastStream().setCloseErr(errors.New("edit refused")) }
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}
	_ = rcv.Receive(context.Background(), conversation.ChannelMessage{SessionID: "s-close", Text: "q"})
	sent := ch.sentCopy()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "cut off") {
		t.Fatalf("sent = %+v, want one cut-off notice after the closing edit failed", sent)
	}
}

type ctxProbeKey struct{}

// probeChannel records whether Send saw a value the caller put on Receive's
// context. The GitHub channel relies on that to mark "a reply was posted"
// (review-20261013-383f #2).
type probeChannel struct {
	stubChannel
	sawValue bool
}

func (p *probeChannel) Send(ctx context.Context, m conversation.ChannelMessage) (string, error) {
	p.sawValue = ctx.Value(ctxProbeKey{}) != nil
	return p.stubChannel.Send(ctx, m)
}

func TestOneShotSend_RunsOnAContextDerivedFromReceive(t *testing.T) {
	ch := &probeChannel{stubChannel: stubChannel{name: "stub"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: &stubAgent{processResult: Result{Text: "hi"}}}
	ctx := context.WithValue(context.Background(), ctxProbeKey{}, true)
	if err := rcv.Receive(ctx, conversation.ChannelMessage{SessionID: "s-probe", Text: "q"}); err != nil {
		t.Fatal(err)
	}
	if !ch.sawValue {
		t.Fatal("Channel.Send ran on a context that does not descend from Receive's")
	}
}

package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
)

// audit 2026-10-12: GitHub #142 §S3 #5 (outcome-quality LLD §S5 #5). The UI,
// Telegram and Slack bound their own turn contexts; the email poll loop and
// the GitHub webhook handed Receive a context with no deadline, so an email
// or GitHub turn was bounded only per call, never as a whole.

// deadlineAgent records the turn context it was handed. When block is set it
// waits for that context to end, the way a looping turn ends at its deadline.
// No sleeps: a 1ns turn bound has already expired when the agent starts.
type deadlineAgent struct {
	block       bool
	hasDeadline bool
	deadline    time.Time
}

func (a *deadlineAgent) Process(ctx context.Context, _ Request) Result {
	a.deadline, a.hasDeadline = ctx.Deadline()
	if a.block {
		if !a.hasDeadline {
			return Result{Err: errors.New("turn ran with no deadline: a looping turn here never ends")}
		}
		<-ctx.Done()
		return Result{Err: ctx.Err()}
	}
	return Result{Text: "ok"}
}

func (a *deadlineAgent) ProcessStreaming(ctx context.Context, req Request, _ chat.StreamCallback) Result {
	return a.Process(ctx, req)
}

func turnMsg() conversation.ChannelMessage {
	return conversation.ChannelMessage{Source: "email", SessionID: "s1", SpeakerID: "a@b", Text: "hi"}
}

func TestReceive_TurnWithoutADeadlineGetsTheDefaultBound(t *testing.T) {
	agent := &deadlineAgent{}
	r := &ChannelReceiver{Channel: &stubChannel{name: "email"}, Agent: agent}
	before := time.Now()
	if err := r.Receive(context.Background(), turnMsg()); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	after := time.Now()
	if !agent.hasDeadline {
		t.Fatal("the turn ran with no deadline: an email or GitHub turn was bounded by nothing")
	}
	if agent.deadline.Before(before.Add(DefaultChannelTurnTimeout)) || agent.deadline.After(after.Add(DefaultChannelTurnTimeout)) {
		t.Errorf("turn deadline %s after start, want DefaultChannelTurnTimeout (%s)", agent.deadline.Sub(before), DefaultChannelTurnTimeout)
	}
}

func TestReceive_CallerDeadlineIsKept(t *testing.T) {
	agent := &deadlineAgent{}
	r := &ChannelReceiver{Channel: &stubChannel{name: "ui"}, Agent: agent, TurnTimeout: time.Hour}
	want := time.Now().Add(5 * time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if err := r.Receive(ctx, turnMsg()); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if !agent.deadline.Equal(want) {
		t.Errorf("turn deadline = %v, want the caller's own %v (UI/Telegram/Slack bound their turns themselves)", agent.deadline, want)
	}
}

func TestReceive_TurnBoundNamesItselfAndWrapsTheSentinel(t *testing.T) {
	agent := &deadlineAgent{block: true}
	r := &ChannelReceiver{Channel: &stubChannel{name: "email"}, Agent: agent, TurnTimeout: time.Nanosecond}
	err := r.Receive(context.Background(), turnMsg())
	if err == nil {
		t.Fatal("a turn ended by its bound returned nil")
	}
	if !errors.Is(err, conversation.ErrTurnDeadline) {
		t.Errorf("err %q does not wrap conversation.ErrTurnDeadline, so the email channel cannot tell it from a transient failure", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %q does not wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "1ns") {
		t.Errorf("err %q does not name the limit", err)
	}
}

// A single model call that timed out inside a live turn is an ordinary
// failure, not the turn's own deadline: the email channel keeps retrying it.
func TestReceive_InnerCallTimeoutIsNotTheTurnDeadline(t *testing.T) {
	agent := &stubAgent{processResult: Result{Err: context.DeadlineExceeded}}
	r := &ChannelReceiver{Channel: &stubChannel{name: "email"}, Agent: agent}
	err := r.Receive(context.Background(), turnMsg())
	if err == nil {
		t.Fatal("want the inner error back")
	}
	if errors.Is(err, conversation.ErrTurnDeadline) {
		t.Errorf("an inner call's timeout was reported as the turn deadline: %v", err)
	}
}

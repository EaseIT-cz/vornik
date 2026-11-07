package dispatcher

// GitHub #645 (dispatcher turn-context design §9). Found at: production,
// 2026-11-04: corrections sent during a looping turn waited behind it and
// read as ignored.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// steerProbeProvider asks for one tool, then answers; it records every call's
// messages and runs a hook before answering.
type steerProbeProvider struct {
	ctxCapturingProvider
	calls  [][]chat.Message
	before func(call int)
}

func (p *steerProbeProvider) CompleteWithToolsStream(ctx context.Context, msgs []chat.Message, tools []chat.Tool, onText chat.StreamCallback) (*chat.ChatResponse, error) {
	r, err := p.CompleteWithTools(ctx, msgs, tools)
	if onText != nil && r.Choices[0].Message.Content != "" {
		onText(r.Choices[0].Message.Content)
	}
	return r, err
}

func (p *steerProbeProvider) CompleteWithTools(_ context.Context, msgs []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	n := len(p.calls)
	p.calls = append(p.calls, append([]chat.Message(nil), msgs...))
	if p.before != nil {
		p.before(n)
	}
	msg := chat.Message{Role: "assistant", Content: "done"}
	if n == 0 {
		msg = chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "c1", Type: "function", Function: chat.FunctionCall{Name: "alpha_tool", Arguments: "{}"}}}}
	}
	r := &chat.ChatResponse{Model: "m"}
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: msg})
	return r, nil
}

func lastUser(msgs []chat.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func TestSteering_JoinsAtNextStep_645(t *testing.T) {
	inbox := NewSteerInbox()
	p := &steerProbeProvider{before: func(call int) {
		if call == 0 {
			inbox.Offer("use the other project", nil)
		}
	}}
	a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(5))
	res := a.Process(WithSteering(context.Background(), inbox), Request{Messages: []chat.Message{{Role: "user", Content: "go"}}, Project: "assistant"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(p.calls) < 2 || lastUser(p.calls[1]) != steerPrefix+"use the other project" {
		t.Fatalf("the second model call must carry the steered message, got %q", lastUser(p.calls[len(p.calls)-1]))
	}
	found := false
	for _, m := range res.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, steerPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("a steered message is part of the turn's saved messages")
	}
}

// A message that arrives while the model gives its final answer keeps the
// turn going, so it is answered in this turn.
func TestSteering_AtFinalAnswerContinues_645(t *testing.T) {
	inbox := NewSteerInbox()
	p := &steerProbeProvider{before: func(call int) {
		if call == 1 {
			inbox.Offer("and also check Brno", nil)
		}
	}}
	a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(5))
	res := a.Process(WithSteering(context.Background(), inbox), Request{Messages: []chat.Message{{Role: "user", Content: "go"}}, Project: "assistant"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(p.calls) != 3 || lastUser(p.calls[2]) != steerPrefix+"and also check Brno" {
		t.Fatalf("calls = %d; the turn must continue with the new message", len(p.calls))
	}
}

func TestSteerInbox_CloseReturnsLeftovers_645(t *testing.T) {
	in := NewSteerInbox()
	in.Offer("a", "ref-a")
	_ = in.Drain()
	in.Offer("b", "ref-b")
	left := in.Close(false)
	if len(left) != 1 || left[0] != "ref-b" {
		t.Fatalf("leftovers = %v, want the undrained one", left)
	}
	if in.Offer("c", "ref-c") {
		t.Fatal("an offer after Close must be refused")
	}
}

// After a failed turn, a steered message that was read but not answered is
// handed back; an answered one is not (review cd48 F2).
func TestSteerInbox_FailureRequeuesUnanswered_645(t *testing.T) {
	in := NewSteerInbox()
	in.Offer("answered", "ref-1")
	_ = in.Drain()
	in.markAnswered()
	in.Offer("read, not answered", "ref-2")
	_ = in.Drain()
	left := in.Close(true)
	if len(left) != 1 || left[0] != "ref-2" {
		t.Fatalf("leftovers after failure = %v, want only the unanswered one", left)
	}
}

// At the last iteration a final answer does not take in new messages; they
// stay in the inbox for their own turn (review cd48 F3).
func TestSteering_NotDrainedAtCap_645(t *testing.T) {
	inbox := NewSteerInbox()
	p := &steerProbeProvider{before: func(call int) {
		if call == 1 {
			inbox.Offer("late", "ref-late")
		}
	}}
	a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(2))
	res := a.Process(WithSteering(context.Background(), inbox), Request{Messages: []chat.Message{{Role: "user", Content: "go"}}, Project: "assistant"})
	if res.Err != nil || res.Text != "done" {
		t.Fatalf("res = %+v", res)
	}
	if left := inbox.Close(false); len(left) != 1 {
		t.Fatalf("the late message must stay for its own turn, got %v", left)
	}
}

// On a streamed reply, continuing after a final answer shows a marker first
// (review cd48 F1, 20eb).
func TestSteering_StreamMarker_645(t *testing.T) {
	inbox := NewSteerInbox()
	p := &steerProbeProvider{before: func(call int) {
		if call == 1 {
			inbox.Offer("one more thing", nil)
		}
	}}
	var streamed string
	a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(5))
	res := a.ProcessStreaming(WithSteering(context.Background(), inbox),
		Request{Messages: []chat.Message{{Role: "user", Content: "go"}}, Project: "assistant"},
		func(acc string) { streamed = acc })
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(streamed, "[Taking your new message into account…]") {
		t.Fatalf("the stream must say the answer is being reconsidered: %q", streamed)
	}
}

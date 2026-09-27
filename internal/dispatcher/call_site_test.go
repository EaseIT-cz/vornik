package dispatcher

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// The dispatcher's chat turns are labelled (ledger-completeness design §13,
// 2026-09-26). Before, they logged `call_site: unknown` beside the agents'
// calls, and the log could not tell a chat turn from a workflow step.

type ctxCapturingProvider struct{ lastCtx context.Context }

func (p *ctxCapturingProvider) Complete(ctx context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	p.lastCtx = ctx
	return &chat.ChatResponse{}, nil
}
func (p *ctxCapturingProvider) CompleteWithTools(ctx context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	p.lastCtx = ctx
	return &chat.ChatResponse{}, nil
}
func (p *ctxCapturingProvider) CompleteWithToolsStream(ctx context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	p.lastCtx = ctx
	return &chat.ChatResponse{}, nil
}
func (p *ctxCapturingProvider) Model() string            { return "stub" }
func (p *ctxCapturingProvider) SetMetrics(*chat.Metrics) {}

func TestDoChatCall_LabelsTheTurn(t *testing.T) {
	for _, stream := range []bool{false, true} {
		p := &ctxCapturingProvider{}
		a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(1))
		var onText chat.StreamCallback
		if stream {
			onText = func(string) {}
		}
		if _, err := a.doChatCall(context.Background(), "sys", nil, nil, onText); err != nil {
			t.Fatal(err)
		}
		if got := chat.CallSiteFromContext(p.lastCtx); got != "dispatcher.turn" {
			t.Fatalf("stream=%v: call site = %q, want dispatcher.turn", stream, got)
		}
	}
	p := &ctxCapturingProvider{}
	a := NewAgent(p, nil, nil, nil, nil, WithMaxIterations(1))
	if _, err := a.doChatCall(chat.WithCallSite(context.Background(), "chat.remember.ned"), "sys", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := chat.CallSiteFromContext(p.lastCtx); got != "chat.remember.ned" {
		t.Fatalf("a caller's own label wins, got %q", got)
	}
}

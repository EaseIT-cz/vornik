package dispatcher

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// GitHub #620 (nonswarm-llm-fallback design §11.2(c)). Found at: production,
// 2026-11-05: had the chat model failed over, the user would not have known a
// different model answered. A turn answered by the twin carries the fail-over
// on its Result so the channel can say so.

type twinAnsweredProvider struct{ ctxCapturingProvider }

func (p *twinAnsweredProvider) resp() *chat.ChatResponse {
	r := &chat.ChatResponse{Model: "zai.glm-5", FallbackFrom: "deepseek-v4-pro:0813"}
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop"})
	return r
}
func (p *twinAnsweredProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return p.resp(), nil
}
func (p *twinAnsweredProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, onText chat.StreamCallback) (*chat.ChatResponse, error) {
	if onText != nil {
		onText("hello")
	}
	return p.resp(), nil
}

func TestTurnAnsweredByTwinCarriesTheFallback_620(t *testing.T) {
	for _, stream := range []bool{false, true} {
		a := NewAgent(&twinAnsweredProvider{}, nil, nil, nil, nil, WithMaxIterations(2))
		req := Request{Messages: []chat.Message{{Role: "user", Content: "hi"}}, Project: "assistant"}
		var res Result
		if stream {
			res = a.ProcessStreaming(context.Background(), req, func(string) {})
		} else {
			res = a.Process(context.Background(), req)
		}
		if res.Err != nil {
			t.Fatalf("stream=%v: %v", stream, res.Err)
		}
		if res.ModelFallback == nil || res.ModelFallback.From != "deepseek-v4-pro:0813" || res.ModelFallback.To != "zai.glm-5" {
			t.Fatalf("stream=%v: ModelFallback = %+v, want deepseek-v4-pro:0813 -> zai.glm-5", stream, res.ModelFallback)
		}
	}
}

func TestTurnAnsweredByPrimaryHasNoFallback_620(t *testing.T) {
	a := NewAgent(&ctxCapturingProvider{}, nil, nil, nil, nil, WithMaxIterations(2))
	res := a.Process(context.Background(), Request{Messages: []chat.Message{{Role: "user", Content: "hi"}}, Project: "assistant"})
	if res.ModelFallback != nil {
		t.Fatalf("ModelFallback = %+v on a primary answer", res.ModelFallback)
	}
}

// Review 64a7 finding 6: a second fail-over in the same turn does not replace
// the first.
func TestFirstModelFallbackKeepsTheFirst_620(t *testing.T) {
	first := firstModelFallback(nil, &chat.ChatResponse{Model: "zai.glm-5", FallbackFrom: "deepseek-v4-pro:0813"})
	got := firstModelFallback(first, &chat.ChatResponse{Model: "other", FallbackFrom: "x"})
	if got != first || got.To != "zai.glm-5" {
		t.Fatalf("got %+v, want the first fail-over kept", got)
	}
	if firstModelFallback(nil, &chat.ChatResponse{Model: "m"}) != nil {
		t.Fatal("a primary answer recorded a fail-over")
	}
}

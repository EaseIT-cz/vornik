package modelcheck

// GitHub #881, project and swarm registry design §12.2 C1: one completion
// through the router with one tool offered, router fallbacks off. An error
// refuses with the provider's reason (naming a missing route when there is
// none); an answer served by another model refuses; a text-only answer passes
// with a warning; a pass is cached 10 minutes by model and route, a refusal
// never.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

type stubCompleter struct {
	mu      sync.Mutex
	calls   map[string]int
	answer  func(model string) (*chat.ChatResponse, error)
	sawCtx  []context.Context
	sawTool []string
}

func (s *stubCompleter) WithModel(m string) Completer {
	return &stubBound{s: s, model: m}
}

type stubBound struct {
	s     *stubCompleter
	model string
}

func (b *stubBound) CompleteWithTools(ctx context.Context, _ []chat.Message, tools []chat.Tool) (*chat.ChatResponse, error) {
	b.s.mu.Lock()
	if b.s.calls == nil {
		b.s.calls = map[string]int{}
	}
	b.s.calls[b.model]++
	b.s.sawCtx = append(b.s.sawCtx, ctx)
	for _, t := range tools {
		b.s.sawTool = append(b.s.sawTool, t.Function.Name)
	}
	b.s.mu.Unlock()
	return b.s.answer(b.model)
}

func respond(model string, toolCall bool) *chat.ChatResponse {
	r := &chat.ChatResponse{Model: model}
	msg := chat.Message{Role: "assistant", Content: "the time is now"}
	if toolCall {
		msg = chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "1", Type: "function", Function: chat.FunctionCall{Name: "get_time", Arguments: "{}"}}}}
	}
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: msg})
	return r
}

func newChecker(s *stubCompleter, routes map[string]string) *Checker {
	return &Checker{Provider: s, RouteOf: func(m string) string { return routes[m] }}
}

func TestCheck_881(t *testing.T) {
	gone := &chat.GatewayError{Status: 410, Message: "retired"}
	s := &stubCompleter{answer: func(m string) (*chat.ChatResponse, error) {
		switch m {
		case "good":
			return respond("good", true), nil
		case "chatty":
			return respond("chatty", false), nil
		case "retired":
			return nil, gone
		case "unrouted":
			return nil, errors.New("no provider")
		case "swapped":
			return respond("someone-else", true), nil
		case "unlisted":
			return respond("unlisted", true), nil // completes though the router lists no route
		}
		return nil, errors.New("unexpected")
	}}
	c := newChecker(s, map[string]string{"good": "ollama", "chatty": "ollama", "retired": "ollama", "swapped": "ollama"})
	ctx := context.Background()

	for model, want := range map[string]struct {
		ok           bool
		warn, reason string
	}{
		"good":     {ok: true},
		"chatty":   {ok: true, warn: "without calling the offered tool"},
		"retired":  {reason: "410"},
		"unrouted": {reason: "no route serves unrouted"},
		"swapped":  {reason: "served by someone-else"},
		"unlisted": {ok: true},
	} {
		r := c.Check(ctx, model)
		if r.OK != want.ok || !strings.Contains(r.Warning, want.warn) || !strings.Contains(r.Reason, want.reason) {
			t.Errorf("%s: %+v, want ok=%v warn~%q reason~%q", model, r, want.ok, want.warn, want.reason)
		}
	}
	for _, sctx := range s.sawCtx {
		if !chat.ModelFallbackSkipped(sctx) {
			t.Error("a probe ran with router fallbacks on: a twin could answer for a dead model")
		}
	}
	if len(s.sawTool) == 0 || s.sawTool[0] != "get_time" {
		t.Errorf("tools offered: %v", s.sawTool)
	}
}

func TestCheckCache_881(t *testing.T) {
	fail := true
	s := &stubCompleter{answer: func(m string) (*chat.ChatResponse, error) {
		if fail {
			return nil, errors.New("down")
		}
		return respond(m, true), nil
	}}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	c := newChecker(s, map[string]string{"m": "ollama"})
	c.now = func() time.Time { return now }
	ctx := context.Background()

	if c.Check(ctx, "m").OK {
		t.Fatal("down model passed")
	}
	fail = false
	if !c.Check(ctx, "m").OK {
		t.Fatal("a refusal was cached: the retry after a fix failed")
	}
	c.Check(ctx, "m")
	if s.calls["m"] != 2 {
		t.Errorf("calls = %d, want 2: a pass is cached", s.calls["m"])
	}
	now = now.Add(11 * time.Minute)
	c.Check(ctx, "m")
	if s.calls["m"] != 3 {
		t.Errorf("calls = %d after 11 minutes, want 3: the cache lasts 10", s.calls["m"])
	}
}

func TestCheckAll_881(t *testing.T) {
	s := &stubCompleter{answer: func(m string) (*chat.ChatResponse, error) {
		if m == "bad" {
			return nil, errors.New("down")
		}
		return respond(m, true), nil
	}}
	c := newChecker(s, map[string]string{"a": "r", "b": "r", "bad": "r"})
	res := c.CheckAll(context.Background(), []string{"a", "b", "bad", "a", ""})
	if len(res) != 3 || !res["a"].OK || !res["b"].OK || res["bad"].OK {
		t.Fatalf("CheckAll = %+v", res)
	}
	if s.calls["a"] != 1 {
		t.Errorf("a duplicate model was probed %d times", s.calls["a"])
	}
}

// A provider that cannot pin a model must not be probed on its default model.
func TestFromProviderUnpinnable_881(t *testing.T) {
	c := &Checker{Provider: FromProvider(nil)}
	if r := c.Check(context.Background(), "m"); r.OK || !strings.Contains(r.Reason, "cannot be pinned") {
		t.Fatalf("%+v", r)
	}
}

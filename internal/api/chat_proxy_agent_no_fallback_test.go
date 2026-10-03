package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
)

// Agent-administered design §18.6 item 2 in detail, round 2 F1 and review
// a125: the router's own model_fallbacks can never reroute an agent call.
// The chat proxy marks every agent-container request WithoutModelFallback,
// so an agent role whose (local) model's circuit is open gets
// MODEL_UNHEALTHY, and the configured twin (here a remote model nobody
// approved for the namespace) is never called.

// pinnedModels is a router stand-in: WithModel pins a model, the primary's
// circuit is open, and every model that is actually called is recorded.
type pinnedModels struct {
	model string
}

func (p *pinnedModels) WithModel(m string) chat.Provider { return &pinnedModels{model: m} }
func (p *pinnedModels) Model() string                    { return p.model }
func (p *pinnedModels) SetMetrics(*chat.Metrics)         {}
func (p *pinnedModels) Complete(ctx context.Context, m []chat.Message) (*chat.ChatResponse, error) {
	return p.CompleteWithTools(ctx, m, nil)
}
func (p *pinnedModels) CompleteWithToolsStream(ctx context.Context, m []chat.Message, t []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return p.CompleteWithTools(ctx, m, t)
}
func (p *pinnedModels) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	modelCalls.mu.Lock()
	modelCalls.called = append(modelCalls.called, p.model)
	modelCalls.mu.Unlock()
	if p.model == "qwen3:35b" {
		return nil, &chat.ModelUnhealthyError{Route: "http", Model: "qwen3:35b", State: "open", OpenSince: time.Now()}
	}
	return stubChatProviderWithUsage{model: p.model, respModel: p.model}.build(), nil
}

var modelCalls struct {
	mu     sync.Mutex
	called []string
}

func TestChatProxy_AgentCallIsNeverServedByTheRouterFallback(t *testing.T) {
	modelCalls.mu.Lock()
	modelCalls.called = nil
	modelCalls.mu.Unlock()
	root := &pinnedModels{model: "qwen3:35b"}
	prov := chat.NewFallbackProvider(root, map[string]string{"qwen3:35b": "google/gemini-pro"}, zerolog.Nop())
	s := NewServer(WithChatProvider(prov))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3:35b","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Vornik-Task-ID", "task_1")
	req.Header.Set("X-Vornik-Execution-ID", "exec_1")
	rec := httptest.NewRecorder()
	s.ChatCompletions(rec, req)

	modelCalls.mu.Lock()
	called := append([]string(nil), modelCalls.called...)
	modelCalls.mu.Unlock()
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "MODEL_UNHEALTHY") {
		t.Fatalf("an agent call whose model's circuit is open answered %d %s (models called %v)", rec.Code, rec.Body.String(), called)
	}
	for _, m := range called {
		if m == "google/gemini-pro" {
			t.Fatalf("the router's model_fallbacks served an agent call on %s", m)
		}
	}
	if len(called) == 0 {
		t.Fatal("the primary was never called")
	}
}

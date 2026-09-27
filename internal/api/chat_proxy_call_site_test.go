package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// The proxy labels every call it forwards, from the same test that decides
// who bills it (ledger-completeness design §13, 2026-09-26). Incident: the
// slow-hardware bench arm logged 41 LLM calls in two hours as
// `call_site: unknown`, so the log could not say what was loading the slow
// backend.

type ctxUsageProvider struct {
	stubChatProviderWithUsage
	lastCtx context.Context
}

func (p *ctxUsageProvider) Complete(ctx context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	p.lastCtx = ctx
	return p.build(), nil
}
func (p *ctxUsageProvider) CompleteWithTools(ctx context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	p.lastCtx = ctx
	return p.build(), nil
}
func (p *ctxUsageProvider) CompleteWithToolsStream(ctx context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	p.lastCtx = ctx
	return p.build(), nil
}
func (p *ctxUsageProvider) Model() string            { return p.model }
func (p *ctxUsageProvider) SetMetrics(*chat.Metrics) {}

func TestChatProxy_CallSiteAndBillingComeFromOneTest(t *testing.T) {
	cases := []struct {
		name      string
		task, exe string
		wantSite  string
		wantBills bool
	}{
		{"task id only", "task_1", "", "agent.step", false},
		{"execution id only", "", "exec_1", "agent.step", false},
		{"both", "task_1", "exec_1", "agent.step", false},
		{"neither", "", "", "chat.external", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &recordingUsageRepo{}
			prov := &ctxUsageProvider{stubChatProviderWithUsage: stubChatProviderWithUsage{model: "m", respModel: "m", promptToks: 10, completToks: 5}}
			s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
			if c.task != "" {
				req.Header.Set("X-Vornik-Task-ID", c.task)
			}
			if c.exe != "" {
				req.Header.Set("X-Vornik-Execution-ID", c.exe)
			}
			if got := isInternalAgentRequest(req); got == c.wantBills {
				t.Fatalf("isInternalAgentRequest = %v for %s", got, c.name)
			}
			s.ChatCompletions(httptest.NewRecorder(), req)
			if prov.lastCtx == nil {
				t.Fatal("the provider was not called")
			}
			if got := chat.CallSiteFromContext(prov.lastCtx); got != c.wantSite {
				t.Fatalf("call site = %q, want %q", got, c.wantSite)
			}
			billed := false
			for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				repo.mu.Lock()
				billed = len(repo.rows) > 0
				repo.mu.Unlock()
				if billed {
					break
				}
			}
			if billed != c.wantBills {
				t.Fatalf("billed here = %v, want %v: the label %q must name the row that accounts for the call", billed, c.wantBills, c.wantSite)
			}
		})
	}
}

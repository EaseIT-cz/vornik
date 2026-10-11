package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GitHub #100 (2026-10-03, task_20261003135506_c7bec90cfe425d9e): the role's
// maxTokens, stamped on the context by the chat proxy, reached only Bedrock.
// Every Client route (http, vertex, openrouter, ollama_cloud) and the Claude
// subscription route sent their construction-time cap instead. The rule now:
// the smaller of the request value and the route cap; the request value alone
// when the route has no cap. Design: 09-agent-runtime-contract.md §8.4,
// amendment 2026-10-08 (D5).

func TestEffectiveMaxTokens(t *testing.T) {
	cases := []struct {
		name     string
		ctxValue int
		routeCap int
		want     int
	}{
		{"no request value keeps the route cap", 0, 4096, 4096},
		{"no request value and no cap omits", 0, 0, 0},
		{"request below the cap wins", 1024, 4096, 1024},
		{"request above the cap is clamped", 9000, 4096, 4096},
		{"request with no route cap is used", 1024, 0, 1024},
		{"equal values", 4096, 4096, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithRequestMaxTokens(context.Background(), tc.ctxValue)
			if got := EffectiveMaxTokens(ctx, tc.routeCap); got != tc.want {
				t.Fatalf("EffectiveMaxTokens(ctx=%d, cap=%d) = %d, want %d", tc.ctxValue, tc.routeCap, got, tc.want)
			}
		})
	}
}

// recordingCompletionServer answers one OpenAI-shaped completion and keeps
// the request body it received.
func recordingCompletionServer(t *testing.T, sink *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sink, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sentMaxTokens(t *testing.T, body []byte) int {
	t.Helper()
	var sent ChatRequest
	if err := json.Unmarshal(bytes.TrimSpace(body), &sent); err != nil {
		t.Fatalf("decode wire body: %v (body %q)", err, body)
	}
	return sent.MaxTokens
}

func TestClient_RequestMaxTokensFromContextIsSent(t *testing.T) {
	var body []byte
	srv := recordingCompletionServer(t, &body)
	c := NewClient(srv.URL, "k", "m", WithMaxTokens(4096))
	ctx := WithRequestMaxTokens(context.Background(), 1024)

	if _, err := c.CompleteWithTools(ctx, []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if got := sentMaxTokens(t, body); got != 1024 {
		t.Fatalf("CompleteWithTools max_tokens = %d, want 1024 (the request value under the 4096 cap)", got)
	}
	if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := sentMaxTokens(t, body); got != 1024 {
		t.Fatalf("Complete max_tokens = %d, want 1024", got)
	}
}

func TestClient_RequestMaxTokensClampedToRouteCap(t *testing.T) {
	var body []byte
	srv := recordingCompletionServer(t, &body)
	c := NewClient(srv.URL, "k", "m", WithMaxTokens(4096))
	ctx := WithRequestMaxTokens(context.Background(), 9000)

	if _, err := c.CompleteWithTools(ctx, []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if got := sentMaxTokens(t, body); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096 (the route cap is the ceiling)", got)
	}
}

func TestClient_RequestMaxTokensWithoutRouteCapIsSent(t *testing.T) {
	var body []byte
	srv := recordingCompletionServer(t, &body)
	c := NewClient(srv.URL, "k", "m")
	ctx := WithRequestMaxTokens(context.Background(), 1024)

	if _, err := c.CompleteWithTools(ctx, []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if got := sentMaxTokens(t, body); got != 1024 {
		t.Fatalf("max_tokens = %d, want 1024 (no route cap: the request value)", got)
	}
}

func TestClientStream_RequestMaxTokensFromContextIsSent(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := &Client{endpoint: srv.URL, model: "m", maxTokens: 4096, httpClient: &http.Client{}}
	ctx := WithRequestMaxTokens(context.Background(), 1024)

	_, _ = c.CompleteWithToolsStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if got, _ := captured["max_tokens"].(float64); int(got) != 1024 {
		t.Fatalf("stream max_tokens = %v, want 1024", captured["max_tokens"])
	}
}

// The openrouter route is a Client behind the free_only wrapper; the wrapper
// must hand the context through unchanged.
func TestOpenRouterFreeOnly_RequestMaxTokensFromContextIsSent(t *testing.T) {
	var body []byte
	srv := recordingCompletionServer(t, &body)
	p := NewFreeOnlyProvider(NewClient(srv.URL, "k", "vendor/model:free", WithMaxTokens(4096)))
	ctx := WithRequestMaxTokens(context.Background(), 1024)

	if _, err := p.CompleteWithTools(ctx, []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if got := sentMaxTokens(t, body); got != 1024 {
		t.Fatalf("max_tokens = %d, want 1024", got)
	}
}

func claudeBodyMaxTokens(ctx context.Context, t *testing.T, c *ClaudeSubscriptionClient) int {
	t.Helper()
	raw, err := c.buildRequestBodyCtx(ctx, []Message{{Role: "user", Content: "hi"}}, nil, claudeAccountInfo{}, "s")
	if err != nil {
		t.Fatalf("buildRequestBodyCtx: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	got, _ := body["max_tokens"].(float64)
	return int(got)
}

func TestClaudeSubscription_RequestMaxTokensFromContextIsSent(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6", WithClaudeSubscriptionMaxTokens(4096))
	if got := claudeBodyMaxTokens(WithRequestMaxTokens(context.Background(), 1024), t, c); got != 1024 {
		t.Fatalf("max_tokens = %d, want 1024", got)
	}
}

func TestClaudeSubscription_RequestMaxTokensClampedToRouteCap(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6", WithClaudeSubscriptionMaxTokens(4096))
	if got := claudeBodyMaxTokens(WithRequestMaxTokens(context.Background(), 9000), t, c); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", got)
	}
}

func TestClaudeSubscription_RequestMaxTokensWithoutRouteCapIsSent(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	if got := claudeBodyMaxTokens(WithRequestMaxTokens(context.Background(), 16000), t, c); got != 16000 {
		t.Fatalf("max_tokens = %d, want 16000 (no route cap: the request value, not the 8192 default)", got)
	}
	if got := claudeBodyMaxTokens(context.Background(), t, c); got != claudeDefaultMaxTokens {
		t.Fatalf("max_tokens = %d, want the default %d when neither is set", got, claudeDefaultMaxTokens)
	}
}

// With extended thinking the API requires max_tokens > budget_tokens; a role
// cap at or below the budget must not turn every call into a 400.
func TestClaudeSubscription_RequestMaxTokensBelowThinkingBudgetIsIgnored(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6",
		WithClaudeSubscriptionMaxTokens(16000), WithClaudeSubscriptionThinkingBudget(4000))
	if got := claudeBodyMaxTokens(WithRequestMaxTokens(context.Background(), 2000), t, c); got != 16000 {
		t.Fatalf("max_tokens = %d, want 16000 (role cap below the thinking budget is not used)", got)
	}
	if got := claudeBodyMaxTokens(WithRequestMaxTokens(context.Background(), 8000), t, c); got != 8000 {
		t.Fatalf("max_tokens = %d, want 8000 (above the budget, under the cap)", got)
	}
}

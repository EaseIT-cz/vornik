package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// GitHub #869: the proxy copied response_format, max_tokens and
// cache_strategy from the agent's body onto the context, and not a reasoning
// level, so no agent role could set one. "none" is the OpenAI-compatible
// spelling of off and arrives as chat.ReasoningEffortOff.
//
// Found at: production (companion reviews failed on the output cap).
// Regression test at: unit. Lower because: the copy is decided here; the
// agent side has its own shell test.
func TestChatCompletions_ReasoningEffortPropagates_869(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`"reasoning_effort":"low"`, chat.ReasoningEffortLow},
		{`"reasoning_effort":"none"`, chat.ReasoningEffortOff},
		{`"reasoning_effort":"off"`, chat.ReasoningEffortOff},
		{`"reasoning_effort":"bogus"`, ""},
	} {
		captured := &ctxCapturingStub{respModel: "stub", resp: buildOllamaOKResponse("stub", "ok")}
		s := NewServer(WithChatProvider(captured))
		body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],` + tc.body + `}`)
		rr := httptest.NewRecorder()
		s.ChatCompletions(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.body, rr.Code)
		}
		if got := chat.ReasoningEffortFromContext(captured.lastCtx); got != tc.want {
			t.Errorf("%s: level on the context = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// End to end through the proxy and a real OpenAI-compatible client: what the
// agent sends is what the upstream receives, off as "none", an unknown value
// not at all (slice 1 code review asked for the wire, not only the context).
func TestChatCompletions_ReasoningEffortReachesTheWire_869(t *testing.T) {
	for _, tc := range []struct {
		sent, want string
		present    bool
	}{
		{"high", "high", true},
		{"off", "none", true},
		{"none", "none", true},
		{"bogus", "", false},
	} {
		var upstream map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &upstream)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		s := NewServer(WithChatProvider(chat.NewClient(srv.URL, "k", "m")))
		body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + tc.sent + `"}`)
		rr := httptest.NewRecorder()
		s.ChatCompletions(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))
		srv.Close()
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", tc.sent, rr.Code, rr.Body.String())
		}
		got, ok := upstream["reasoning_effort"]
		if ok != tc.present || (ok && got != tc.want) {
			t.Errorf("sent %q: upstream reasoning_effort = %v (present %v), want %q (present %v)", tc.sent, got, ok, tc.want, tc.present)
		}
	}
}

// B6: the agent learns how much the model wrote as reasoning, from any
// provider that returns the text (Ollama Cloud's message.reasoning here).
func TestChatCompletions_ReportsReasoningBytes_869(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning":"0123456789"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":8192,"total_tokens":8193}}`))
	}))
	defer srv.Close()
	s := NewServer(WithChatProvider(chat.NewClient(srv.URL, "k", "m")))
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Usage struct {
			ReasoningBytes int `json:"reasoning_bytes"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Usage.ReasoningBytes != 10 {
		t.Errorf("usage.reasoning_bytes = %d, want 10", out.Usage.ReasoningBytes)
	}
}

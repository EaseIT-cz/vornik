package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// GitHub #667 (Ollama-compatible API design, change 2026-11-07). The Ollama
// layer answered every provider error with err.Error(); on 2026-11-07 a
// buffered /api/chat returned the upstream account name and the provider's
// links to the caller. Both proxies now share one mapping, and only
// daemon-built text is ever forwarded.

const acct667 = "you (acct-667) have reached your weekly usage limit, upgrade: https://provider.example/upgrade"

func TestProviderErrorReply667_EachClass(t *testing.T) {
	unhealthy := &chat.ModelUnhealthyError{Route: "r1", Model: "m1", State: "open", OpenSince: time.Unix(0, 0).UTC()}
	overflow := &chat.RouteOverflowError{Route: "r1", Depth: 4, Waited: time.Second}
	cases := []struct {
		name               string
		err                error
		status             int
		code, msg, notHave string
	}{
		{"route overflow", overflow, 503, "ROUTE_OVERFLOW", overflow.Error(), ""},
		{"circuit open", unhealthy, 503, "MODEL_UNHEALTHY", unhealthy.Error(), ""},
		{"circuit open under a wrapper with provider text", fmt.Errorf("%s: %w", acct667, unhealthy), 503, "MODEL_UNHEALTHY", unhealthy.Error(), "acct-667"},
		{"context overflow", errors.New("This model's maximum context length is 8192 tokens"), 400, "CONTEXT_OVERFLOW", "", "8192"},
		{"typed 429", &chat.GatewayError{Status: 429, Message: acct667}, 429, chat.CodeUpstreamRateLimited, "", "acct-667"},
		{"typed 503 with an account name", &chat.GatewayError{Status: 503, Message: "down for (acct-667)"}, 502, "PROVIDER_ERROR", "upstream provider returned an error", "acct-667"},
	}
	for _, c := range cases {
		status, code, msg := providerErrorReply(c.err)
		if status != c.status || code != c.code {
			t.Errorf("%s: got %d %s, want %d %s", c.name, status, code, c.status, c.code)
		}
		if c.msg != "" && msg != c.msg {
			t.Errorf("%s: message %q, want %q", c.name, msg, c.msg)
		}
		if c.notHave != "" && strings.Contains(msg, c.notHave) {
			t.Errorf("%s: message forwards provider text: %q", c.name, msg)
		}
	}
}

func ollamaErrServer667(err error) *Server {
	return NewServer(WithChatProvider(&streamingStub{model: "stub", err: err}))
}

// Buffered /api/chat and /api/generate: class status and code, no provider text.
func TestOllamaBuffered667_NoProviderText(t *testing.T) {
	for _, path := range []string{"/api/chat", "/api/generate"} {
		for _, c := range []struct {
			err    error
			status int
			code   string
		}{
			{&chat.GatewayError{Status: 429, Message: acct667}, http.StatusTooManyRequests, chat.CodeUpstreamRateLimited},
			{&chat.GatewayError{Status: 502, Message: "bad gateway for (acct-667)"}, http.StatusBadGateway, "PROVIDER_ERROR"},
		} {
			s := ollamaErrServer667(c.err)
			body := `{"stream":false,"messages":[{"role":"user","content":"hi"}]}`
			if path == "/api/generate" {
				body = `{"stream":false,"prompt":"hi"}`
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
			if path == "/api/chat" {
				s.OllamaChat(rr, req)
			} else {
				s.OllamaGenerate(rr, req)
			}
			if rr.Code != c.status || !strings.Contains(rr.Body.String(), `"code":"`+c.code+`"`) {
				t.Errorf("%s %d: got %d %s", path, c.status, rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "acct-667") || strings.Contains(rr.Body.String(), "provider.example") {
				t.Errorf("%s: the reply forwards provider text: %s", path, rr.Body.String())
			}
		}
	}
}

func lastDoneReason667(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	scanner := bufio.NewScanner(rr.Body)
	var last struct {
		Done       bool   `json:"done"`
		DoneReason string `json:"done_reason"`
	}
	for scanner.Scan() {
		_ = json.Unmarshal(scanner.Bytes(), &last)
	}
	if !last.Done {
		t.Fatal("the error frame must have done:true")
	}
	return last.DoneReason
}

// Streamed errors: the final frame names the code once and carries no
// provider text.
func TestOllamaStream667_NoProviderText(t *testing.T) {
	unhealthy := &chat.ModelUnhealthyError{Route: "r1", Model: "m1", State: "open", OpenSince: time.Unix(0, 0).UTC()}
	overflow := &chat.RouteOverflowError{Route: "r1", Depth: 4, Waited: time.Second}
	for _, path := range []string{"/api/chat", "/api/generate"} {
		for _, c := range []struct {
			err        error
			wantPrefix string
		}{
			{&chat.GatewayError{Status: 502, Message: "bad gateway for (acct-667)"}, "error: PROVIDER_ERROR: upstream provider returned an error"},
			{&chat.GatewayError{Status: 429, Message: acct667}, "error: " + chat.CodeUpstreamRateLimited + ": the model provider refused"},
			{overflow, "error: ROUTE_OVERFLOW: chat route"},
			{unhealthy, "error: MODEL_UNHEALTHY: model"},
		} {
			s := ollamaErrServer667(c.err)
			body := `{"messages":[{"role":"user","content":"hi"}]}`
			if path == "/api/generate" {
				body = `{"prompt":"hi"}`
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
			if path == "/api/chat" {
				s.OllamaChat(rr, req)
			} else {
				s.OllamaGenerate(rr, req)
			}
			dr := lastDoneReason667(t, rr)
			if !strings.HasPrefix(dr, c.wantPrefix) {
				t.Errorf("%s: done_reason %q, want prefix %q", path, dr, c.wantPrefix)
			}
			if strings.Contains(dr, "acct-667") || strings.Count(dr, "MODEL_UNHEALTHY") > 1 || strings.Count(dr, chat.CodeUpstreamRateLimited) > 1 {
				t.Errorf("%s: done_reason %q forwards provider text or repeats the code", path, dr)
			}
		}
	}
}

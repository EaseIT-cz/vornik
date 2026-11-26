package ui

// GitHub #862 (web chat design §2.4, "Amendment 2026-11-25 (#862)"): a failed
// turn's page showed "Chat dispatch failed: " + the dispatcher's raw error, to
// every viewer. That error can carry an upstream response body, an endpoint
// URL or a key fragment; the security model promises non-operators scrubbed
// error text, and the Telegram notice never quotes it. The page now shows the
// same classified notice the streaming channels show; the raw error goes to
// the daemon log.
//
// Found at: review (the #862 as-built read, 2026-11-25), observed at
// integration (the web chat's HTTP handler; a user sees it in the browser).

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/dispatcher"
)

const leakyErr862 = "upstream 401 from https://llm.internal.example:8443/v1: Incorrect API key provided: sk-live-abc123"

type leakyDispatcher862 struct{}

func (leakyDispatcher862) Process(_ context.Context, _ dispatcher.Request) dispatcher.Result {
	return dispatcher.Result{Err: chatTestErr(leakyErr862)}
}

func (d leakyDispatcher862) ProcessStreaming(ctx context.Context, req dispatcher.Request, _ chat.StreamCallback) dispatcher.Result {
	return d.Process(ctx, req)
}

func TestChat862_AFailedTurnNeverShowsTheRawError(t *testing.T) {
	for _, operator := range []bool{false, true} {
		root := writeSwarmFixture(t)
		server, _ := swarmEditServer(t, root)
		server.chatDispatcher = leakyDispatcher862{}
		var logs bytes.Buffer
		server.logger = zerolog.New(&logs)

		form := url.Values{}
		form.Set("prompt", "hi")
		req := httptest.NewRequest(http.MethodPost, "/projects/p1/chat/messages", strings.NewReader(form.Encode()))
		if operator {
			req = asOperator491(req)
		} else {
			req = req.WithContext(api.ContextWithScopeForTesting(req.Context(), "p1")) // a member of p1
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "s862"})
		rec := httptest.NewRecorder()
		server.ChatPostMessage(rec, req, "p1")

		body := rec.Body.String()
		for _, leak := range []string{"sk-live", "llm.internal", "Incorrect API key", "Chat dispatch failed"} {
			if strings.Contains(body, leak) {
				t.Errorf("operator=%v: the page shows %q", operator, leak)
			}
		}
		if !strings.Contains(body, "could not answer") {
			t.Errorf("operator=%v: the page does not say the assistant could not answer", operator)
		}
		// The operator still has the error: the daemon log carries it.
		if !strings.Contains(logs.String(), "sk-live-abc123") || !strings.Contains(logs.String(), `"level":"warn"`) {
			t.Errorf("operator=%v: the raw error is not in the log at Warn: %s", operator, logs.String())
		}
	}
}

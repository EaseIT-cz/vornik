package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
)

// GitHub #665 (agent LLM health breaker design, amendment 2026-11-07): the
// Ollama Cloud weekly usage limit answered HTTP 429, and the proxy hid it
// behind a generic 502 PROVIDER_ERROR, so only 4 of 340 failed steps recorded
// why. A rate or usage limit now has its own code; the upstream text is
// still never forwarded.
func TestChatCompletions665_AnUpstreamLimitHasItsOwnCode(t *testing.T) {
	upstream := &chat.GatewayError{Status: http.StatusTooManyRequests,
		Message: "you (acct-name) have reached your weekly usage limit, upgrade for higher limits: https://provider.example/upgrade"}
	s, _ := exchangeTestServer(t, false, &stubProvider{err: upstream})
	w := httptest.NewRecorder()
	s.ChatCompletions(w, agentRequest(false))

	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	body := w.Body.String()
	require.Contains(t, body, `"code":"UPSTREAM_RATE_LIMITED"`)
	require.Contains(t, body, "UPSTREAM_RATE_LIMITED: the model provider refused the call for a usage or rate limit")
	require.NotContains(t, body, "acct-name", "the upstream text is never forwarded")
	require.NotContains(t, body, "provider.example")
}

// Any other upstream failure keeps the generic envelope.
func TestChatCompletions665_AnUpstream5xxIsStillProviderError(t *testing.T) {
	s, _ := exchangeTestServer(t, false, &stubProvider{err: &chat.GatewayError{Status: http.StatusServiceUnavailable, Message: "down"}})
	w := httptest.NewRecorder()
	s.ChatCompletions(w, agentRequest(false))
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), `"code":"PROVIDER_ERROR"`)
}

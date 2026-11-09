package api

import (
	"errors"
	"net/http"

	"vornik.io/vornik/internal/chat"
)

// providerErrorReply maps a chat provider's error to the reply a caller gets:
// HTTP status, error code and message. It is the one mapping for both the
// OpenAI-compatible proxy and the Ollama-compatible layer (Ollama-compatible
// API design, change 2026-11-07, GitHub #667).
//
// Only daemon-built text is ever forwarded. A provider's own error text echoes
// routing details, account names and links, so it is logged by the caller and
// replaced here by a fixed message. The two classes that keep their text are
// built by the daemon: a route queue overflow (route, depth, wait) and an open
// circuit (model, route, state, time). The circuit text is taken from the
// matched *chat.ModelUnhealthyError, never from err: errors.As matches through
// wrappers, and a wrapper's Error() may carry text of its own.
//
// The order is the OpenAI proxy's original branch order.
func providerErrorReply(err error) (status int, code, message string) {
	// A direct type assertion, as chat.IsRouteOverflow: err is the typed value.
	if overflow, ok := err.(*chat.RouteOverflowError); ok {
		return http.StatusServiceUnavailable, "ROUTE_OVERFLOW", overflow.Error()
	}
	var unhealthy *chat.ModelUnhealthyError
	if errors.As(err, &unhealthy) {
		return http.StatusServiceUnavailable, "MODEL_UNHEALTHY", unhealthy.Error()
	}
	if chat.IsContextOverflow(err) {
		return http.StatusBadRequest, "CONTEXT_OVERFLOW",
			"prompt exceeds the model's context window — compact the conversation or reduce input"
	}
	if chat.IsUpstreamRateLimited(err) {
		return http.StatusTooManyRequests, chat.CodeUpstreamRateLimited,
			chat.CodeUpstreamRateLimited + ": the model provider refused the call for a usage or rate limit"
	}
	return http.StatusBadGateway, "PROVIDER_ERROR", "upstream provider returned an error"
}

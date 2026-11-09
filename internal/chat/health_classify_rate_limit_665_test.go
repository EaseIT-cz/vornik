package chat

import (
	"errors"
	"fmt"
	"testing"
)

// GitHub #665: an upstream usage or rate limit is recognised as such, typed or
// as text, and stays an infra failure for the breaker.
func TestIsUpstreamRateLimited665(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed gateway 429", &GatewayError{Status: 429, Message: "slow down"}, true},
		{"wrapped gateway 429", fmt.Errorf("call: %w", &GatewayError{Status: 429}), true},
		{"typed retryable 429", &retryableHTTPError{StatusCode: 429, Body: "x"}, true},
		{"typed gateway 503", &GatewayError{Status: 503, Message: "down"}, false},
		{"ollama weekly limit text", errors.New("gateway error 429: you have reached your weekly usage limit"), true},
		{"the proxy's own code", errors.New("LLM call failed: " + CodeUpstreamRateLimited + ": the model provider refused the call"), true},
		{"bedrock throttling", errors.New("ThrottlingException: Rate exceeded"), true},
		{"ordinary failure", errors.New("upstream provider returned an error"), false},
	}
	for _, c := range cases {
		if got := IsUpstreamRateLimited(c.err); got != c.want {
			t.Errorf("%s: IsUpstreamRateLimited = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsUpstreamInfraError665_EveryRateLimitMarkerIsInfra(t *testing.T) {
	for _, m := range rateLimitMarkers {
		if !IsUpstreamInfraError(errors.New("x " + m + " y")) {
			t.Errorf("rate-limit marker %q is not infra; the breaker would stop counting it", m)
		}
	}
}

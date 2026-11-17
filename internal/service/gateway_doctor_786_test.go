package service

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #786: the doctor probed the gateway address whether or not the
// gateway was enabled; the shipped example disables it but keeps an address,
// so every fresh instance reported gateway_healthy ERROR.
func TestGatewayDoctorURL786(t *testing.T) {
	c := config.DefaultConfig()
	c.Gateway.Address = "http://127.0.0.1:8010"
	if got := gatewayDoctorURL(c); got != "" {
		t.Fatalf("a disabled gateway is probed at %q; want none (SKIPPED)", got)
	}
	c.Gateway.Enabled = true
	if got := gatewayDoctorURL(c); got != "http://127.0.0.1:8010" {
		t.Fatalf("an enabled gateway: %q", got)
	}
	if gatewayDoctorURL(nil) != "" {
		t.Fatal("nil config")
	}
}

// GitHub #786 (review-20261115-21be F3): the agent environment carries the
// managed default, and the daemon-side config does not move.
func TestAgentContainerLLM786(t *testing.T) {
	c := config.DefaultConfig()
	c.Hosting.Mode = config.HostingModeManaged
	c.Server.Address = ":8080"
	c.Chat.Endpoint = "https://gateway.internal/v1"
	c.Chat.Model = "m"
	got := agentContainerLLM(c)
	if got.Endpoint != "http://host.containers.internal:8080/api/v1" || got.Model != "m" {
		t.Fatalf("agent LLM on a managed instance: %+v", got)
	}
	if c.ResolvedAgentLLM().Endpoint != "https://gateway.internal/v1" {
		t.Fatal("the daemon-side resolved endpoint moved")
	}
}

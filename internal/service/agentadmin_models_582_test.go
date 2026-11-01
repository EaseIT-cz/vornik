package service

// GitHub #582 P3: only the provisioner's model hosts, in managed hosting, mark
// a route provided.

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

func TestProvidedRoute582(t *testing.T) {
	managed := config.HostingConfig{Mode: config.HostingModeManaged, ModelHosts: []string{"gateway.internal"}}
	self := config.HostingConfig{ModelHosts: nil}
	cases := []struct {
		name string
		h    config.HostingConfig
		ep   string
		want bool
	}{
		{"the gateway", managed, "https://gateway.internal/v1", true},
		{"case-insensitive host", managed, "https://GATEWAY.internal/v1", true},
		{"plain http", managed, "http://gateway.internal/v1", false},
		{"another host", managed, "https://api.openai.com/v1", false},
		{"self hosting", self, "https://gateway.internal/v1", false},
		{"no endpoint", managed, "", false},
	}
	for _, c := range cases {
		if got := providedRoute(c.h, c.ep); got != c.want {
			t.Errorf("%s: providedRoute = %v, want %v", c.name, got, c.want)
		}
	}
}

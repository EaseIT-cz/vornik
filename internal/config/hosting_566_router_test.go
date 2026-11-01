package config

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/mcpauth"
)

// GitHub #566 follow-up (mcp-server-authentication-design.md §16.5 C1, c31d
// F3): with the EaseIT OAuth router configured, a daemon-level oauth server
// must name its router provider and EaseIT's client, or its sign-in could
// never complete; such a config does not start. A provider name is managed
// hosting's only.
func TestConfig566_ManagedOAuthServerNeedsAProvider(t *testing.T) {
	routed := func(auth mcpauth.Auth) *Config {
		c := managedConfig()
		c.Server.PublicBaseURL = "https://acme.vornik.cloud"
		c.Hosting.InstanceID = "acme"
		c.Hosting.OAuthRedirectURI = "https://connect.vornik.cloud/oauth/callback"
		c.MCP.Servers = []MCPServerConfig{{Name: "cal", Transport: "streamable-http", URL: "https://mcp.example.com/mcp", Auth: auth}}
		return c
	}
	if err := routed(mcpauth.Auth{Mode: mcpauth.ModeOAuth, ClientID: "easeit", RouterProvider: "google"}).Validate(); err != nil {
		t.Fatalf("a routed server with a provider and EaseIT's client: %v", err)
	}
	for want, auth := range map[string]mcpauth.Auth{
		"auth.router_provider": {Mode: mcpauth.ModeOAuth, ClientID: "easeit"},
		"auth.client_id":       {Mode: mcpauth.ModeOAuth, RouterProvider: "google"},
	} {
		if err := routed(auth).Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err %v; want one naming %s", auth, err, want)
		}
	}
	// A static server needs neither.
	if err := routed(mcpauth.Auth{Mode: mcpauth.ModeStatic, ValueFrom: "secret://k"}).Validate(); err != nil {
		t.Errorf("a static server on a routed instance: %v", err)
	}
	// A provider name on a self host is refused.
	self := baseValidConfig()
	self.MCP.Servers = []MCPServerConfig{{Name: "cal", Transport: "streamable-http", URL: "https://mcp.example.com/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth, RouterProvider: "google"}}}
	if err := self.Validate(); err == nil || !strings.Contains(err.Error(), "router_provider") {
		t.Errorf("a router provider on a self host: err %v", err)
	}
}

func TestConfig566_ARoutedManualEndpointNeedsAnIssuer(t *testing.T) {
	c := managedConfig()
	c.Server.PublicBaseURL = "https://acme.vornik.cloud"
	c.Hosting.InstanceID = "acme"
	c.Hosting.OAuthRedirectURI = "https://connect.vornik.cloud/oauth/callback"
	c.MCP.Servers = []MCPServerConfig{{Name: "cal", Transport: "streamable-http", URL: "https://mcp.example.com/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth, ClientID: "easeit", RouterProvider: "google",
			AuthorizationEndpoint: "https://as.example/a", TokenEndpoint: "https://as.example/t"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "auth.issuer") {
		t.Fatalf("err %v; want a refusal naming auth.issuer", err)
	}
	c.MCP.Servers[0].Auth.Issuer = "https://as.example"
	if err := c.Validate(); err != nil {
		t.Fatalf("with the issuer: %v", err)
	}
}

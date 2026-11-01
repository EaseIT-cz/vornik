package config

// GitHub #582 P1 (operations-api-contract-design.md §6.2 P1): the OpenID
// provider's configuration. Found at: design review (6.1 B4: only GitHub
// exists, and it verifies no ID token).

import (
	"strings"
	"testing"
)

func oidcConfig() *Config {
	c := baseValidConfig()
	c.Server.PublicBaseURL = "https://acme.vornik.cloud"
	c.Auth.Providers.OIDC = &OIDCProviderSettings{Issuer: "https://vornik.cloud", ClientID: "inst-582"}
	return c
}

func TestOIDCProvider582_Validation(t *testing.T) {
	c := oidcConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("a complete OpenID provider must load: %v", err)
	}
	if got := c.Auth.Providers.OIDC.ChannelName(); got != "vornikcloud" {
		t.Errorf("default channel name = %q, want vornikcloud", got)
	}
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"no public origin", func(c *Config) { c.Server.PublicBaseURL = ""; c.Auth.ExternalBaseURL = "" }, "public origin"},
		{"no issuer", func(c *Config) { c.Auth.Providers.OIDC.Issuer = "" }, "auth.providers.oidc.issuer"},
		{"plain-http issuer", func(c *Config) { c.Auth.Providers.OIDC.Issuer = "http://vornik.cloud" }, "auth.providers.oidc.issuer"},
		{"issuer with a query", func(c *Config) { c.Auth.Providers.OIDC.Issuer = "https://vornik.cloud/?x=1" }, "auth.providers.oidc.issuer"},
		{"no client id", func(c *Config) { c.Auth.Providers.OIDC.ClientID = "" }, "auth.providers.oidc.client_id"},
		{"a name with a colon", func(c *Config) { c.Auth.Providers.OIDC.Name = "a:b" }, "auth.providers.oidc.name"},
		{"the github name", func(c *Config) { c.Auth.Providers.OIDC.Name = "github" }, "auth.providers.oidc.name"},
	}
	for _, tc := range cases {
		c := oidcConfig()
		tc.mut(c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
}

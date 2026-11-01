package config

import (
	"strings"
	"testing"
)

// GitHub #566, SaaS MVP M7 daemon half (18-security-and-tenancy.md,
// "Amendment 2026-10-28 (#566, M7 daemon half)", E1 and E4;
// mcp-server-authentication-design.md §16 R1). Found at: design review; the
// level an operator observes is a config the daemon loads or refuses.

func TestImageServers566_Validation(t *testing.T) {
	refused := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"a name", func(c *Config) { c.Hosting.ImageServers = []string{"localhost:8931"} }, "hosting.image_servers"},
		{"another loopback address", func(c *Config) { c.Hosting.ImageServers = []string{"127.0.0.2:8931"} }, "hosting.image_servers"},
		{"no port", func(c *Config) { c.Hosting.ImageServers = []string{"127.0.0.1"} }, "hosting.image_servers"},
		{"port 0", func(c *Config) { c.Hosting.ImageServers = []string{"127.0.0.1:0"} }, "hosting.image_servers"},
		{"a scheme", func(c *Config) { c.Hosting.ImageServers = []string{"http://127.0.0.1:8931"} }, "hosting.image_servers"},
		{"a duplicate", func(c *Config) { c.Hosting.ImageServers = []string{"127.0.0.1:8931", "127.0.0.1:8931"} }, "hosting.image_servers"},
		{"a private address", func(c *Config) { c.Hosting.ImageServers = []string{"10.0.0.7:8931"} }, "hosting.image_servers"},
		{"the daemon's port behind a wildcard bind", func(c *Config) {
			c.Server.Address = "0.0.0.0:8080"
			c.Hosting.ImageServers = []string{"127.0.0.1:8080"}
		}, "hosting.image_servers"},
		{"the daemon's port, empty host", func(c *Config) {
			c.Server.Address = ":8080"
			c.Hosting.ImageServers = []string{"[::1]:8080"}
		}, "hosting.image_servers"},
		{"the daemon's port, loopback bind", func(c *Config) {
			c.Server.Address = "127.0.0.1:8080"
			c.Hosting.ImageServers = []string{"127.0.0.1:8080"}
		}, "hosting.image_servers"},
		{"the operations listener's port", func(c *Config) { c.Hosting.ImageServers = []string{"127.0.0.1:9443"} }, "hosting.image_servers"},
		{"the daemon's port by service name", func(c *Config) {
			c.Server.Address = "127.0.0.1:http"
			c.Hosting.ImageServers = []string{"127.0.0.1:80"}
		}, "hosting.image_servers"},
		{"the metrics listener's port", func(c *Config) {
			c.Metrics.Addr = ":9090"
			c.Hosting.ImageServers = []string{"127.0.0.1:9090"}
		}, "hosting.image_servers"},
		{"a daemon server on an unlisted loopback pair", func(c *Config) {
			c.MCP.Servers = []MCPServerConfig{{Name: "gws", Transport: "streamable-http", URL: "http://127.0.0.1:8931/mcp"}}
		}, "mcp.servers"},
		{"a daemon server on localhost", func(c *Config) {
			c.Hosting.ImageServers = []string{"127.0.0.1:8931"}
			c.MCP.Servers = []MCPServerConfig{{Name: "gws", Transport: "streamable-http", URL: "http://localhost:8931/mcp"}}
		}, "mcp.servers"},
		{"a daemon server on a private literal", func(c *Config) {
			c.MCP.Servers = []MCPServerConfig{{Name: "x", Transport: "sse", URL: "http://10.0.0.5:80/sse"}}
		}, "mcp.servers"},
		{"a daemon server on the metadata address", func(c *Config) {
			c.MCP.Servers = []MCPServerConfig{{Name: "x", Transport: "sse", URL: "http://169.254.169.254/sse"}}
		}, "mcp.servers"},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			c := managedConfig()
			tc.mut(c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error naming %s", err, tc.want)
			}
		})
	}
	t.Run("refused/in self mode", func(t *testing.T) {
		c := baseValidConfig()
		c.Hosting.ImageServers = []string{"127.0.0.1:8931"}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "hosting.image_servers") {
			t.Fatalf("Validate() = %v, want hosting.image_servers refused in self mode", err)
		}
	})
	accepted := []struct {
		name string
		mut  func(*Config)
	}{
		{"an IPv4 pair with its server", func(c *Config) {
			c.Hosting.ImageServers = []string{"127.0.0.1:8931"}
			c.MCP.Servers = []MCPServerConfig{{Name: "gws", Transport: "streamable-http", URL: "http://127.0.0.1:8931/mcp"}}
		}},
		{"an IPv6 pair", func(c *Config) { c.Hosting.ImageServers = []string{"[::1]:8931"} }},
		{"the port an unconfigured metrics listener would use", func(c *Config) {
			c.Metrics.Addr = ""
			c.Hosting.ImageServers = []string{"127.0.0.1:9090"}
		}},
		{"a public daemon server", func(c *Config) {
			c.MCP.Servers = []MCPServerConfig{{Name: "x", Transport: "sse", URL: "https://mcp.example.com/sse"}}
		}},
	}
	for _, tc := range accepted {
		t.Run("accepted/"+tc.name, func(t *testing.T) {
			c := managedConfig()
			tc.mut(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestHostingOAuthRouter566_Config(t *testing.T) {
	router := func(c *Config) {
		c.Server.PublicBaseURL = "https://acme.vornik.cloud"
		c.Hosting.InstanceID = "acme"
		c.Hosting.OAuthRedirectURI = "https://connect.vornik.cloud/oauth/callback"
	}
	refused := []struct {
		name string
		mut  func(*Config)
	}{
		{"no instance id", func(c *Config) { c.Hosting.InstanceID = "" }},
		{"an id with a dot", func(c *Config) { c.Hosting.InstanceID = "ac.me" }},
		{"http", func(c *Config) { c.Hosting.OAuthRedirectURI = "http://connect.vornik.cloud/oauth/callback" }},
		{"a query", func(c *Config) { c.Hosting.OAuthRedirectURI = "https://connect.vornik.cloud/oauth/callback?x=1" }},
		{"a fragment", func(c *Config) { c.Hosting.OAuthRedirectURI = "https://connect.vornik.cloud/oauth/callback#f" }},
		{"userinfo", func(c *Config) { c.Hosting.OAuthRedirectURI = "https://u:p@connect.vornik.cloud/oauth/callback" }},
		{"an empty path", func(c *Config) { c.Hosting.OAuthRedirectURI = "https://connect.vornik.cloud" }},
		{"another parent domain", func(c *Config) { c.Hosting.OAuthRedirectURI = "https://evil.example/cb" }},
		{"a look-alike suffix", func(c *Config) { c.Hosting.OAuthRedirectURI = "https://connect.evilvornik.cloud/cb" }},
		{"no public base URL", func(c *Config) { c.Server.PublicBaseURL = "" }},
		{"an apex instance host", func(c *Config) {
			c.Server.PublicBaseURL = "https://vornik.cloud"
			c.Hosting.OAuthRedirectURI = "https://other.cloud/cb"
		}},
		{"a two-label instance host", func(c *Config) {
			c.Server.PublicBaseURL = "https://acme.com"
			c.Hosting.OAuthRedirectURI = "https://router.com/cb"
		}},
		{"a host under a multi-label suffix", func(c *Config) {
			c.Server.PublicBaseURL = "https://acme.co.uk"
			c.Hosting.OAuthRedirectURI = "https://router.co.uk/cb"
		}},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			c := managedConfig()
			router(c)
			tc.mut(c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "hosting.") {
				t.Fatalf("Validate() = %v, want a hosting.* refusal", err)
			}
		})
	}
	t.Run("refused/in self mode", func(t *testing.T) {
		c := baseValidConfig()
		router(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "hosting.") {
			t.Fatalf("Validate() = %v, want the router keys refused in self mode", err)
		}
	})
	for name, uri := range map[string]string{
		"a router under the parent": "https://connect.vornik.cloud/oauth/callback",
		"the parent itself":         "https://vornik.cloud/oauth/callback",
	} {
		t.Run("accepted/"+name, func(t *testing.T) {
			c := managedConfig()
			router(c)
			c.Hosting.OAuthRedirectURI = uri
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
	t.Run("accepted/an id without the router", func(t *testing.T) {
		c := managedConfig()
		c.Hosting.InstanceID = "acme"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})
	if InstanceIDPattern.MatchString("ac.me") || !InstanceIDPattern.MatchString("acme-2") {
		t.Fatal("InstanceIDPattern must refuse a dot and accept the provisioner's ids")
	}
}

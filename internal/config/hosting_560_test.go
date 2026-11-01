package config

import (
	"strings"
	"testing"
)

// GitHub #560 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1a)", A1): managed hosting refuses at load every configuration that
// would leave the public listener or the operations listener unsafe.

func managedConfig() *Config {
	c := baseValidConfig()
	c.API.AuthEnabled = true
	c.API.APIKeys = []string{"sk-plain-key", "sk-instance-admin"}
	c.Admin.Enabled = true
	c.Admin.AllowedKeys = []string{"sk-instance-admin"}
	c.Hosting.Mode = HostingModeManaged
	c.Hosting.OperationsAddress = "10.0.0.5:9443"
	c.Metrics.RequireAdmin = true
	// M1c D1: the managed defaults the provisioner writes.
	c.API.RateLimit.PerIP.RPS = 20
	c.API.RateLimit.PerIP.Burst = 40
	c.Server.RealIP.Enabled = true
	c.Server.RealIP.TrustedProxies = []string{"10.0.1.0/24"}
	c.Hosting.ModelHosts = []string{"gateway.internal"}
	return c
}

// M1c D1: the managed defaults are refused at load when missing or unsafe.
func TestHostingConfig560_ManagedDefaults(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		// GitHub #561 D5a.
		{"admin key not authenticating", func(c *Config) { c.API.APIKeys = []string{"sk-plain-key"} }, "every admin.allowed_keys entry in api.api_keys"},
		{"no per-IP rps", func(c *Config) { c.API.RateLimit.PerIP.RPS = 0 }, "api.rate_limit.per_ip"},
		{"no per-IP burst", func(c *Config) { c.API.RateLimit.PerIP.Burst = 0 }, "api.rate_limit.per_ip"},
		{"real_ip off", func(c *Config) { c.Server.RealIP.Enabled = false }, "server.real_ip"},
		{"no trusted proxy", func(c *Config) { c.Server.RealIP.TrustedProxies = nil }, "server.real_ip.trusted_proxies"},
		{"public proxy", func(c *Config) { c.Server.RealIP.TrustedProxies = []string{"198.51.100.0/24"} }, "server.real_ip.trusted_proxies"},
		{"universal proxy", func(c *Config) { c.Server.RealIP.TrustedProxies = []string{"0.0.0.0/0"} }, "server.real_ip.trusted_proxies"},
		{"broad private proxy", func(c *Config) { c.Server.RealIP.TrustedProxies = []string{"10.0.0.0/8"} }, "server.real_ip.trusted_proxies"},
		{"loopback proxy", func(c *Config) { c.Server.RealIP.TrustedProxies = []string{"127.0.0.1/32"} }, "server.real_ip.trusted_proxies"},
		{"link-local proxy", func(c *Config) { c.Server.RealIP.TrustedProxies = []string{"169.254.0.0/16"} }, "server.real_ip.trusted_proxies"},
		{"broad v6 proxy", func(c *Config) { c.Server.RealIP.TrustedProxies = []string{"fd00::/8"} }, "server.real_ip.trusted_proxies"},
		{"stdio MCP by transport", func(c *Config) {
			c.MCP.Servers = []MCPServerConfig{{Name: "s", Transport: "stdio", Command: "x"}}
		}, "mcp.servers"},
		{"MCP with empty transport", func(c *Config) {
			c.MCP.Servers = []MCPServerConfig{{Name: "s", URL: "https://m.example.com"}}
		}, "mcp.servers"},
		{"HTTP MCP with a command", func(c *Config) {
			c.MCP.Servers = []MCPServerConfig{{Name: "s", Transport: "sse", URL: "https://m.example.com", Command: "x"}}
		}, "mcp.servers"},
		{"model host IP", func(c *Config) { c.Hosting.ModelHosts = []string{"10.0.0.5"} }, "hosting.model_hosts"},
		{"model host wildcard", func(c *Config) { c.Hosting.ModelHosts = []string{"*.internal"} }, "hosting.model_hosts"},
		{"model host with scheme", func(c *Config) { c.Hosting.ModelHosts = []string{"https://gw.internal"} }, "hosting.model_hosts"},
		{"model host with port", func(c *Config) { c.Hosting.ModelHosts = []string{"gw.internal:443"} }, "hosting.model_hosts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := managedConfig()
			tc.mut(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error naming %s", err, tc.want)
			}
		})
	}
	ok := managedConfig()
	ok.Server.RealIP.TrustedProxies = []string{"10.0.1.7", "10.0.2.0/24", "fd00:1::/64"}
	ok.MCP.Servers = []MCPServerConfig{{Name: "gws", Transport: "streamable-http", URL: "https://mcp.vornik.cloud/gws"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a complete managed config was refused: %v", err)
	}
	self := baseValidConfig()
	self.Hosting.ModelHosts = []string{"gw.internal"}
	if err := self.Validate(); err == nil || !strings.Contains(err.Error(), "hosting.model_hosts") {
		t.Fatalf("model_hosts in self mode: %v", err)
	}
}

func TestHostingConfig560_Validate(t *testing.T) {
	if err := baseValidConfig().Validate(); err != nil {
		t.Fatalf("an empty hosting block must load: %v", err)
	}
	if err := managedConfig().Validate(); err != nil {
		t.Fatalf("a valid managed config must load: %v", err)
	}
	for _, ok := range []string{"10.0.0.5:9443", "127.0.0.1:9443", "[fd00::5]:9443", "192.168.1.9:9443", "172.16.0.2:1"} {
		c := managedConfig()
		c.Hosting.OperationsAddress = ok
		if err := c.Validate(); err != nil {
			t.Errorf("operations address %q refused: %v", ok, err)
		}
	}

	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"auth off", func(c *Config) { c.API.AuthEnabled = false }, "api.auth_enabled"},
		{"no operations address", func(c *Config) { c.Hosting.OperationsAddress = "" }, "hosting.operations_address"},
		{"operations address is server.address", func(c *Config) { c.Hosting.OperationsAddress = c.Server.Address }, "hosting.operations_address"},
		{"wildcard v4", func(c *Config) { c.Hosting.OperationsAddress = "0.0.0.0:9443" }, "hosting.operations_address"},
		{"wildcard v6", func(c *Config) { c.Hosting.OperationsAddress = "[::]:9443" }, "hosting.operations_address"},
		{"empty host", func(c *Config) { c.Hosting.OperationsAddress = ":9443" }, "hosting.operations_address"},
		{"link-local", func(c *Config) { c.Hosting.OperationsAddress = "169.254.0.1:9443" }, "hosting.operations_address"},
		{"host name", func(c *Config) { c.Hosting.OperationsAddress = "ops.internal:9443" }, "hosting.operations_address"},
		{"public IP", func(c *Config) { c.Hosting.OperationsAddress = "198.51.100.7:9443" }, "hosting.operations_address"},
		{"no port", func(c *Config) { c.Hosting.OperationsAddress = "10.0.0.5" }, "hosting.operations_address"},
		{"port held by an all-interfaces server.address", func(c *Config) {
			c.Server.Address = ":8080"
			c.Hosting.OperationsAddress = "127.0.0.1:8080"
		}, "hosting.operations_address"},
		{"port held by 0.0.0.0", func(c *Config) {
			c.Server.Address = "0.0.0.0:8080"
			c.Hosting.OperationsAddress = "10.0.0.5:8080"
		}, "hosting.operations_address"},
		{"relay ingress", func(c *Config) { c.Node.RelayIngress.Addr = ":8443" }, "node.relay_ingress"},
		{"metrics without require_admin", func(c *Config) { c.Metrics.RequireAdmin = false }, "metrics.require_admin"},
		{"metrics enabled without require_admin", func(c *Config) { c.Metrics.Enabled = true; c.Metrics.RequireAdmin = false }, "metrics.require_admin"},
		{"admin disabled", func(c *Config) { c.Admin.Enabled = false }, "admin.enabled"},
		{"no admin key", func(c *Config) { c.Admin.AllowedKeys = nil }, "admin.allowed_keys"},
		{"unknown mode", func(c *Config) { c.Hosting.Mode = "hosted" }, "hosting.mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := managedConfig()
			tc.mut(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error naming %s", err, tc.want)
			}
		})
	}

	// An operations address in self mode is refused: nobody asked for a
	// second, ungated listener.
	c := baseValidConfig()
	c.Hosting.OperationsAddress = "10.0.0.5:9443"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "hosting.operations_address") {
		t.Fatalf("self mode with an operations address: Validate() = %v", err)
	}
	c = baseValidConfig()
	c.Hosting.Mode = HostingModeSelf
	if err := c.Validate(); err != nil {
		t.Fatalf("explicit self mode must load: %v", err)
	}
	if !managedConfig().Hosting.Managed() || baseValidConfig().Hosting.Managed() {
		t.Fatal("Managed() must be true only for hosting.mode: managed")
	}
}

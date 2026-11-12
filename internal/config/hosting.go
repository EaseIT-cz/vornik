package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"

	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/mcpauth"
)

// validateHosting refuses every managed-hosting configuration that would
// leave a listener unsafe (GitHub #560; 18-security-and-tenancy.md,
// "Amendment 2026-10-28 (#560, slice M1a)", A1).
func (c *Config) validateHosting() error {
	h := c.Hosting
	switch h.Mode {
	case "", HostingModeSelf:
		if h.OperationsAddress != "" {
			return errors.New("hosting.operations_address is set but hosting.mode is not managed: the operations listener exists only in managed hosting")
		}
		if len(h.ModelHosts) > 0 {
			return errors.New("hosting.model_hosts is set but hosting.mode is not managed")
		}
		if len(h.ImageServers) > 0 || h.InstanceID != "" || h.OAuthRedirectURI != "" {
			return errors.New("hosting.image_servers, hosting.instance_id and hosting.oauth_redirect_uri are set but hosting.mode is not managed")
		}
		if h.PreviousAdminAPIKey != "" {
			return errors.New("hosting.previous_admin_api_key is set but hosting.mode is not managed: the rotation operation is managed hosting's")
		}
		if h.BackupBucket != "" || h.BackupRegion != "" {
			return errors.New("hosting.backup_bucket and hosting.backup_region are set but hosting.mode is not managed: the owner export is managed hosting's")
		}
		for _, srv := range c.MCP.Servers {
			if srv.Auth.RouterProvider != "" {
				return fmt.Errorf("mcp.servers %q: auth.router_provider is set but hosting.mode is not managed: the EaseIT OAuth router is managed hosting's", srv.Name)
			}
		}
		return nil
	case HostingModeManaged:
	default:
		return fmt.Errorf("hosting.mode %q is not one of self, managed", h.Mode)
	}
	if !c.API.AuthEnabled {
		return errors.New("hosting.mode managed requires api.auth_enabled: with auth off every request is admin-class")
	}
	if err := validateOperationsAddress(h.OperationsAddress, c.Server.Address); err != nil {
		return err
	}
	if !c.Admin.Enabled || len(c.Admin.AllowedKeys) == 0 {
		if !c.Admin.Enabled {
			return errors.New("hosting.mode managed requires admin.enabled: instance_admin is an admin.allowed_keys key on the operations listener")
		}
		return errors.New("hosting.mode managed requires at least one admin.allowed_keys entry (instance_admin)")
	}
	// GitHub #561 D5a: an admin.allowed_keys key authenticates only when an
	// auth backend also holds it; the provisioner writes it to api.api_keys.
	for _, k := range c.Admin.AllowedKeys {
		if !slices.Contains(c.API.APIKeys, k) {
			return errors.New("hosting.mode managed requires every admin.allowed_keys entry in api.api_keys: otherwise instance_admin cannot authenticate")
		}
	}
	if err := c.validatePreviousAdminAPIKey(); err != nil {
		return err
	}
	if err := validateBackupBucket(h, c.Storage.S3.Region); err != nil {
		return err
	}
	if c.Node.RelayIngress.Addr != "" {
		return errors.New("hosting.mode managed does not support node.relay_ingress: the job tier is not a hosted shape")
	}
	// The mux serves /metrics whether or not metrics.enabled is set, so the
	// gate is required unconditionally. The dedicated metrics server, which
	// never authenticates, is not started in managed hosting at all
	// (service.observabilityConfig), whatever metrics.addr says.
	if !c.Metrics.RequireAdmin {
		return errors.New("hosting.mode managed requires metrics.require_admin: /metrics then takes metrics.scrape_token or the instance_admin key")
	}
	return c.validateManagedDefaults()
}

// ManagedAgentPidsLimit is the agent container PID limit in managed hosting:
// the value podman applied to every agent on the reference host before
// #569 set one (08-runtime-manager-podman.md, "Change 2026-10-28", H1).
const ManagedAgentPidsLimit = 2048

// Bounds of an explicit agent_pids_limit in managed hosting: 64 is the
// sandbox one-shot's limit, 32768 the kernel's traditional pid_max default.
const (
	minAgentPidsLimit = 64
	maxAgentPidsLimit = 32768
)

// AgentHardening is the effective read-only root and PID limit of an agent
// container (GitHub #569 R1): managed hosting forces them on.
func (r RuntimeConfig) AgentHardening(h HostingConfig) (readOnlyRoot bool, pidsLimit int) {
	if h.Managed() {
		return true, ManagedAgentPidsLimit
	}
	return r.AgentReadOnlyRoot != nil && *r.AgentReadOnlyRoot, r.AgentPidsLimit
}

// validateAgentHardening refuses a negative limit anywhere, and in managed
// hosting an explicit read-only off or an out-of-range limit.
func (c *Config) validateAgentHardening() error {
	r := c.Runtime
	if r.AgentPidsLimit < 0 {
		return fmt.Errorf("runtime.agent_pids_limit %d: 0 leaves podman's default, otherwise a positive limit", r.AgentPidsLimit)
	}
	if !c.Hosting.Managed() {
		return nil
	}
	if r.AgentReadOnlyRoot != nil && !*r.AgentReadOnlyRoot {
		return errors.New("runtime.agent_read_only_root: hosting.mode managed runs agent containers read-only; remove the setting")
	}
	if r.AgentPidsLimit != 0 && (r.AgentPidsLimit < minAgentPidsLimit || r.AgentPidsLimit > maxAgentPidsLimit) {
		return fmt.Errorf("runtime.agent_pids_limit %d: hosting.mode managed accepts %d..%d (and uses %d)", r.AgentPidsLimit, minAgentPidsLimit, maxAgentPidsLimit, ManagedAgentPidsLimit)
	}
	return nil
}

// validateManagedDefaults is M1c's D1: the per-IP limiter, the real client IP
// behind the edge, an HTTP-only daemon MCP catalogue and well-formed vouched
// model hosts (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1c)").
func (c *Config) validateManagedDefaults() error {
	if pi := c.API.RateLimit.PerIP; pi.RPS <= 0 || pi.Burst <= 0 {
		return errors.New("hosting.mode managed requires api.rate_limit.per_ip rps and burst above zero (the provisioner writes 20 and 40)")
	}
	ri := c.Server.RealIP
	if !ri.Enabled {
		return errors.New("hosting.mode managed requires server.real_ip.enabled: behind the edge every client would otherwise key on the load balancer")
	}
	if len(ri.TrustedProxies) == 0 {
		return errors.New("hosting.mode managed requires server.real_ip.trusted_proxies: the edge's subnets")
	}
	for _, p := range ri.TrustedProxies {
		if err := narrowPrivatePrefix(p); err != nil {
			return fmt.Errorf("server.real_ip.trusted_proxies %q: %w", p, err)
		}
	}
	for _, s := range c.MCP.Servers {
		if (s.Transport != "sse" && s.Transport != "streamable-http") || s.Command != "" {
			return fmt.Errorf("mcp.servers %q: hosting.mode managed starts no stdio MCP server; declare transport sse or streamable-http and no command", s.Name)
		}
	}
	if len(c.AgentAdmin.PrivateAPIHosts) > 0 {
		return errors.New("agent_admin.private_api_hosts is set but hosting.mode is managed: a managed instance reaches no customer intranet (GitHub #734)")
	}
	for _, h := range c.Hosting.ModelHosts {
		if err := dialguard.ValidModelHost(h); err != nil {
			return fmt.Errorf("hosting.model_hosts: %w", err)
		}
	}
	pairs, err := c.validImageServers()
	if err != nil {
		return err
	}
	if err := c.validateDaemonMCPHosts(pairs); err != nil {
		return err
	}
	if err := c.validateOAuthRouter(); err != nil {
		return err
	}
	return c.validateRoutedMCPServers()
}

// validateRoutedMCPServers is design §16.5 C1 (c31d F3): with the router
// configured, a daemon-level oauth server names its router provider and
// EaseIT's client, or its sign-in could never complete. A project server is
// refused at begin with the same rule.
func (c *Config) validateRoutedMCPServers() error {
	if c.Hosting.OAuthRedirectURI == "" {
		return nil
	}
	for _, srv := range c.MCP.Servers {
		if srv.Auth.EffectiveMode() != mcpauth.ModeOAuth {
			continue
		}
		if srv.Auth.RouterProvider == "" {
			return fmt.Errorf("mcp.servers %q: a managed instance's sign-ins go through the EaseIT OAuth router, so auth.router_provider is required", srv.Name)
		}
		if srv.Auth.ClientID == "" {
			return fmt.Errorf("mcp.servers %q: the router admits EaseIT's client only, so auth.client_id is required (not dynamic registration)", srv.Name)
		}
		if srv.Auth.AuthorizationEndpoint != "" && srv.Auth.Issuer == "" {
			return fmt.Errorf("mcp.servers %q: a manual endpoint on a managed instance states auth.issuer, so the response's iss can be checked", srv.Name)
		}
	}
	return nil
}

// narrowPrivatePrefix accepts an IP or CIDR wholly inside a private range
// (RFC 1918, ULA) and no wider than /24 (IPv4) or /64 (IPv6).
func narrowPrivatePrefix(s string) error {
	var pfx netip.Prefix
	if ip, err := netip.ParseAddr(s); err == nil {
		pfx = netip.PrefixFrom(ip, ip.BitLen())
	} else if pfx, err = netip.ParsePrefix(s); err != nil {
		return fmt.Errorf("not an IP or CIDR")
	}
	pfx = pfx.Masked()
	limit := 24
	if pfx.Addr().Is6() {
		limit = 64
	}
	if pfx.Bits() < limit {
		return fmt.Errorf("wider than /%d: list the edge's subnets only", limit)
	}
	if !pfx.Addr().IsPrivate() {
		return fmt.Errorf("not inside a private range (RFC 1918, ULA)")
	}
	return nil
}

// validateOperationsAddress requires host:port whose host is a loopback or
// private (RFC 1918, ULA) IP literal: never unspecified, link-local, public or
// a name, so the ungated operations surface is never on every interface.
func validateOperationsAddress(addr, serverAddr string) error {
	if addr == "" {
		return errors.New("hosting.mode managed requires hosting.operations_address")
	}
	if addr == serverAddr {
		return errors.New("hosting.operations_address must differ from server.address")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("hosting.operations_address %q is not host:port: %w", addr, err)
	}
	// server.address on every interface (":8080", "0.0.0.0:8080", "[::]:8080")
	// already holds that port on the operations host too.
	if sHost, sPort, err := net.SplitHostPort(serverAddr); err == nil && sPort == port {
		if sIP, perr := netip.ParseAddr(sHost); sHost == "" || (perr == nil && sIP.IsUnspecified()) {
			return fmt.Errorf("hosting.operations_address %q uses the port server.address %q holds on every interface", addr, serverAddr)
		}
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("hosting.operations_address %q: the host must be an IP literal, loopback or private", addr)
	}
	if !ip.IsLoopback() && !ip.IsPrivate() {
		return fmt.Errorf("hosting.operations_address %q: the host must be a loopback or private address (not unspecified, link-local or public)", addr)
	}
	return nil
}

// validatePreviousAdminAPIKey checks the rotation overlap slot (GitHub #582
// §6.3 F3): the previous key still authenticates, so it is in api.api_keys and
// admin.allowed_keys, and at least one other admin key is current.
func (c *Config) validatePreviousAdminAPIKey() error {
	prev := c.Hosting.PreviousAdminAPIKey
	if prev == "" {
		return nil
	}
	in := func(list []string) bool {
		for _, k := range list {
			if k == prev {
				return true
			}
		}
		return false
	}
	if !in(c.API.APIKeys) || !in(c.Admin.AllowedKeys) {
		return errors.New("hosting.previous_admin_api_key must be listed in both api.api_keys and admin.allowed_keys: it authenticates until it is retired")
	}
	for _, k := range c.Admin.AllowedKeys {
		if k != prev {
			return nil
		}
	}
	return errors.New("hosting.previous_admin_api_key is the only admin.allowed_keys entry: a rotation keeps a current key beside it")
}

// s3BucketName is an S3 bucket name: 3 to 63 lowercase letters, digits, dots
// and hyphens, beginning and ending with a letter or digit.
var s3BucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// awsRegion is an AWS region name, e.g. eu-central-1.
var awsRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)

// validateBackupBucket checks the owner export's bucket (GitHub #581 D5): a
// plain bucket name, a region name, and the instance id its key is built
// from.
func validateBackupBucket(h HostingConfig, storageRegion string) error {
	if h.BackupBucket == "" {
		if h.BackupRegion != "" {
			return errors.New("hosting.backup_region is set without hosting.backup_bucket")
		}
		return nil
	}
	if !s3BucketName.MatchString(h.BackupBucket) {
		return fmt.Errorf("hosting.backup_bucket %q is not an S3 bucket name (3 to 63 lowercase letters, digits, dots and hyphens; no path)", h.BackupBucket)
	}
	if h.BackupRegion != "" && !awsRegion.MatchString(h.BackupRegion) {
		return fmt.Errorf("hosting.backup_region %q is not a region name such as eu-central-1", h.BackupRegion)
	}
	if h.BackupRegion == "" && storageRegion == "" {
		return errors.New("hosting.backup_bucket needs a region: set hosting.backup_region (or storage.s3.region)")
	}
	if h.InstanceID == "" {
		return errors.New("hosting.backup_bucket requires hosting.instance_id: the export key is instances/<hosting.instance_id>/exports/<id>.tar.zst")
	}
	return nil
}

// validateAgentAdminPrivateAPIHosts checks and folds agent_admin.private_api_hosts
// (agent-administered design part 06, §6.7, GitHub #734): bare host names,
// lowercase, no trailing dot.
func (c *Config) validateAgentAdminPrivateAPIHosts() error {
	for i, h := range c.AgentAdmin.PrivateAPIHosts {
		if err := dialguard.ValidModelHost(h); err != nil {
			return fmt.Errorf("agent_admin.private_api_hosts: %w", err)
		}
		c.AgentAdmin.PrivateAPIHosts[i] = dialguard.FoldHost(h)
	}
	return nil
}

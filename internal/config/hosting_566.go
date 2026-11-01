package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"

	"vornik.io/vornik/internal/instanceid"
)

// InstanceIDPattern is the managed instance id rule (GitHub #566, §16 R3),
// shared with the provision command and the routed OAuth state.
var InstanceIDPattern = instanceid.Pattern

var (
	loopback4 = netip.MustParseAddr("127.0.0.1")
	loopback6 = netip.IPv6Loopback()
)

// ImageServerPairs parses hosting.image_servers; Validate has already
// refused any malformed entry, so a parse failure here drops the entry.
func (h HostingConfig) ImageServerPairs() []netip.AddrPort {
	var out []netip.AddrPort
	for _, s := range h.ImageServers {
		if p, err := netip.ParseAddrPort(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// validImageServers is M7 E1: exact loopback literals with a port, no
// duplicate, and never the port of a TCP listener the daemon opens, whatever
// that listener's bind address (18-security-and-tenancy.md, "Amendment
// 2026-10-28 (#566, M7 daemon half)").
func (c *Config) validImageServers() ([]netip.AddrPort, error) {
	listenerPorts := map[string]string{}
	for key, addr := range map[string]string{
		"server.address":             c.Server.Address,
		"hosting.operations_address": c.Hosting.OperationsAddress,
		"metrics.addr":               c.Metrics.Addr,
		"node.relay_ingress.addr":    c.Node.RelayIngress.Addr,
	} {
		if addr == "" {
			continue
		}
		// A service name ("http") is the port net.Listen would bind
		// (review 8c0e F3), so it is resolved before the comparison.
		if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
			if n, err := net.LookupPort("tcp", port); err == nil {
				listenerPorts[fmt.Sprint(n)] = key
			}
		}
	}
	seen := map[netip.AddrPort]bool{}
	var out []netip.AddrPort
	for _, s := range c.Hosting.ImageServers {
		p, err := netip.ParseAddrPort(s)
		if err != nil || (p.Addr() != loopback4 && p.Addr() != loopback6) || p.Port() == 0 {
			return nil, fmt.Errorf("hosting.image_servers %q: an exact 127.0.0.1:<port> or [::1]:<port> pair, a literal address with a port, no name", s)
		}
		if seen[p] {
			return nil, fmt.Errorf("hosting.image_servers %q is listed twice", s)
		}
		if key, ok := listenerPorts[fmt.Sprint(p.Port())]; ok {
			return nil, fmt.Errorf("hosting.image_servers %q: port %d is the daemon's own %s listener", s, p.Port(), key)
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// validateDaemonMCPHosts is M7 E4: a daemon MCP server on a loopback name or
// a non-public IP literal must be a listed pair, or it could never connect.
// A name that resolves to such an address is not known here; it is refused
// at dial time.
func (c *Config) validateDaemonMCPHosts(pairs []netip.AddrPort) error {
	for _, s := range c.MCP.Servers {
		u, err := url.Parse(s.URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return fmt.Errorf("mcp.servers %q: a managed instance never dials localhost; an image server is named by its listed hosting.image_servers address", s.Name)
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || publicAddr(addr) {
			continue
		}
		port := u.Port()
		if port == "" {
			port = map[string]string{"https": "443", "http": "80"}[strings.ToLower(u.Scheme)]
		}
		want, err := netip.ParseAddrPort(net.JoinHostPort(addr.String(), port))
		if err != nil || !containsPair(pairs, want) {
			return fmt.Errorf("mcp.servers %q: %s is not a public address and not a hosting.image_servers pair, so the server could never connect", s.Name, net.JoinHostPort(host, port))
		}
	}
	return nil
}

func publicAddr(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate()
}

func containsPair(pairs []netip.AddrPort, p netip.AddrPort) bool {
	for _, q := range pairs {
		if q == p {
			return true
		}
	}
	return false
}

// validateOAuthRouter is §16 R1: the instance id's pattern, and a router URI
// that is https, has a path and no userinfo, query or fragment, and sits
// under the parent domain of server.public_base_url's host, which must not be
// a public suffix.
func (c *Config) validateOAuthRouter() error {
	h := c.Hosting
	if h.InstanceID != "" && !InstanceIDPattern.MatchString(h.InstanceID) {
		return fmt.Errorf("hosting.instance_id %q: lowercase letters, digits and inner hyphens, at most 64", h.InstanceID)
	}
	if h.OAuthRedirectURI == "" {
		return nil
	}
	if h.InstanceID == "" {
		return errors.New("hosting.oauth_redirect_uri requires hosting.instance_id: a routed state names the instance")
	}
	u, err := url.Parse(h.OAuthRedirectURI)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return fmt.Errorf("hosting.oauth_redirect_uri %q: an https URL with a path and no userinfo, query or fragment", h.OAuthRedirectURI)
	}
	base, err := url.Parse(strings.TrimSpace(c.Server.PublicBaseURL))
	if err != nil || base.Scheme != "https" || base.Hostname() == "" {
		return errors.New("hosting.oauth_redirect_uri requires an https server.public_base_url: the router's domain is checked against the instance's")
	}
	parent, err := registrableParent(base.Hostname())
	if err != nil {
		return fmt.Errorf("hosting.oauth_redirect_uri: %w", err)
	}
	router := strings.ToLower(u.Hostname())
	if router != parent && !strings.HasSuffix(router, "."+parent) {
		return fmt.Errorf("hosting.oauth_redirect_uri host %q is not under %q, the parent domain of server.public_base_url", router, parent)
	}
	return nil
}

// CheckOAuthRedirect is §16 R1 for a router URI that is about to be written
// beside the given origin and instance id (GitHub #591: managed setup checks
// a body before writing it).
func CheckOAuthRedirect(publicBaseURL, instanceID, uri string) error {
	c := Config{Server: ServerConfig{PublicBaseURL: publicBaseURL}, Hosting: HostingConfig{InstanceID: instanceID, OAuthRedirectURI: uri}}
	return c.validateOAuthRouter()
}

// registrableParent is host without its first label, refused when that is a
// public suffix rather than a registrable domain or a name under one.
func registrableParent(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	_, parent, ok := strings.Cut(host, ".")
	if !ok || parent == "" {
		return "", fmt.Errorf("server.public_base_url host %q has no parent domain", host)
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || (parent != etld1 && !strings.HasSuffix(parent, "."+etld1)) {
		return "", fmt.Errorf("server.public_base_url host %q: its parent domain %q is a public suffix, not a registrable domain", host, parent)
	}
	return parent, nil
}

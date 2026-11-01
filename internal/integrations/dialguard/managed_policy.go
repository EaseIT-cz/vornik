package dialguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// ManagedPolicy is managed hosting's outbound policy (GitHub #560;
// 18-security-and-tenancy.md, "Amendment 2026-10-28 (#560, slice M1c)", D0,
// D4, D5). Nil (the default) is self-hosted.
type ManagedPolicy struct {
	// ModelHosts are the model-endpoint host names the provisioner vouches
	// for (the EaseIT model gateway): they may resolve to a private address.
	ModelHosts []string
	// ImageServers are the image's declared loopback MCP servers
	// (hosting.image_servers, GitHub #566 E1).
	ImageServers []netip.AddrPort
}

// managedPolicy is process-wide: one daemon runs one hosting mode. Tests that
// set it reset it with t.Cleanup and must not run in parallel with tests that
// expect the other mode.
var managedPolicy atomic.Pointer[ManagedPolicy]

// SetManagedPolicy sets, or with nil clears, the managed policy. The
// container sets it when it loads a managed config, before any listener
// accepts a request; tests reset it with t.Cleanup.
func SetManagedPolicy(p *ManagedPolicy) { managedPolicy.Store(p) }

// currentManagedPolicy is the policy in force, read on every call so a client
// built before the policy was set still dials under it.
func currentManagedPolicy() *ManagedPolicy { return managedPolicy.Load() }

// ManagedPolicySet reports whether a managed policy is in force. The daemon
// refuses to serve a managed config without one (fail closed, review b0fa F1).
func ManagedPolicySet() bool { return currentManagedPolicy() != nil }

// CustomerURLGuard is the guard for a URL a managed instance's customer can
// steer (an MCP server added through the agent-admin verbs, its OAuth flow):
// managed, DialGuard{AllowedHosts: allowed}, so a host not in allowed reaches
// public addresses only and a loopback-named URL gets no loopback grant;
// self-hosted, LoopbackURLGuard unchanged. allowed is only ever operator
// input (D5).
func CustomerURLGuard(rawURL string, allowed []string) DialGuard {
	if currentManagedPolicy() == nil {
		return LoopbackURLGuard(rawURL, allowed)
	}
	return DialGuard{AllowedHosts: append([]string(nil), allowed...)}
}

// ModelEndpointClient is the embedder's default client: its transport
// resolves ModelEndpointGuard on every dial (D0), consults no proxy, and it
// follows only same-host redirects.
func ModelEndpointClient(timeout time.Duration) *http.Client {
	return liveClient(timeout, ModelEndpointGuard)
}

// CustomerURLClient is a client for a customer-steerable URL: its transport
// resolves CustomerURLGuard on every dial, consults no proxy, and it follows
// only same-host redirects (D5: MCP headers and a 307/308 body do not ride a
// redirect to another host).
//
// Self-hosted (no policy) it behaves as LoopbackURLGuard's client did: the
// same guard and Go's default redirect bound, so a self-hosted install is not
// changed; the same-host rule applies while a managed policy is set, read on
// every redirect.
func CustomerURLClient(rawURL string, allowed []string, timeout time.Duration) *http.Client {
	allowed = append([]string(nil), allowed...)
	c := liveClient(timeout, func() DialGuard { return CustomerURLGuard(rawURL, allowed) })
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if currentManagedPolicy() == nil {
			if len(via) >= maxRedirectRequests {
				return fmt.Errorf("stopped after %d requests", maxRedirectRequests)
			}
			return nil
		}
		return SameHostRedirects(req, via)
	}
	return c
}

func liveClient(timeout time.Duration, guard func() DialGuard) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return guard().DialContext(ctx, network, addr)
	}
	return &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: SameHostRedirects}
}

// ManagedPolicyFor is the policy a config implies: the managed policy for a
// managed config, nil otherwise. The container stores it at construction, so
// the policy follows the config it was built from.
func ManagedPolicyFor(managed bool, modelHosts []string, imageServers ...netip.AddrPort) *ManagedPolicy {
	if !managed {
		return nil
	}
	return &ManagedPolicy{ModelHosts: append([]string(nil), modelHosts...), ImageServers: append([]netip.AddrPort(nil), imageServers...)}
}

// ImageServerGuard is the guard for a URL taken from config.yaml (GitHub
// #566, M7 E2, E3): managed, when the URL's host is an IP literal whose
// address and port (the scheme's default filled in) are a listed image
// server, the guard exempts exactly that pair and checks every other dial
// as CustomerURLGuard does; otherwise it is CustomerURLGuard. Read on every
// dial (D0), so a pair removed from the list is refused at the next dial.
func ImageServerGuard(rawURL string) DialGuard {
	p := currentManagedPolicy()
	if p == nil {
		return LoopbackURLGuard(rawURL, nil)
	}
	if u, err := url.Parse(rawURL); err == nil {
		if addr, err := netip.ParseAddr(u.Hostname()); err == nil {
			port := portOf(u.Scheme, u.Port())
			if want, err := netip.ParseAddrPort(net.JoinHostPort(addr.String(), port)); err == nil {
				for _, listed := range p.ImageServers {
					if listed == want {
						return DialGuard{ExactAddrs: []netip.AddrPort{want}}
					}
				}
			}
		}
	}
	return CustomerURLGuard(rawURL, nil)
}

// ImageServerClient is ImageServerGuard's client: a live read on every dial,
// no proxy, same-host redirects (managed) or Go's default bound (self).
func ImageServerClient(rawURL string, timeout time.Duration) *http.Client {
	c := liveClient(timeout, func() DialGuard { return ImageServerGuard(rawURL) })
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if currentManagedPolicy() == nil {
			if len(via) >= maxRedirectRequests {
				return fmt.Errorf("stopped after %d requests", maxRedirectRequests)
			}
			return nil
		}
		return SameHostRedirects(req, via)
	}
	return c
}

// ValidModelHost reports whether h may be a hosting.model_hosts entry: a bare
// DNS name, never an IP literal, a wildcard, a scheme or a port (D4).
func ValidModelHost(h string) error {
	switch {
	case h == "":
		return fmt.Errorf("empty host")
	case net.ParseIP(h) != nil:
		return fmt.Errorf("%q is an IP literal; name the host", h)
	case strings.ContainsAny(h, "*:/@ \t") || strings.Contains(h, ".."):
		return fmt.Errorf("%q is not a bare host name (no wildcard, scheme, port or path)", h)
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("%q is not a valid host name", h)
		}
		for _, r := range label {
			if !hostNameRune(r) {
				return fmt.Errorf("%q is not a valid host name", h)
			}
		}
	}
	return nil
}

// hostNameRune reports whether r may appear in a DNS label (LDH).
func hostNameRune(r rune) bool {
	return r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

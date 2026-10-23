// Package dialguard is the daemon's resolved-IP SSRF guard for dials of
// configured or operator-supplied URLs (integrations-hub-design §6 and §0.0b).
// It is a leaf package (stdlib only) so that any package, the executor
// included, can dial through it; internal/integrations re-exports its names.
package dialguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// DialGuard is the shared SSRF guard every Prober dials through (design
// §6, security-critical). Probes accept user-supplied endpoints — an MCP
// server URL, a self-hosted IMAP/SMTP host, a GitHub Enterprise base URL —
// and since project-scoped (non-admin) users can trigger a probe, an
// unguarded dial is a dial-out-anywhere primitive. DialGuard blocks RFC1918,
// loopback, and link-local destinations at connect time.
//
// The check runs on the *resolved* IP via net.Dialer.Control, which fires
// after DNS resolution but before the connect(2) syscall — not a hostname
// string check — so a hostname that resolves to a private address (DNS
// rebinding, or simply "localhost") is caught regardless of what the
// hostname string looks like.
//
// AllowedHosts is an opt-in escape hatch (empty = block all private/
// loopback/link-local destinations) for self-hosted operators who need to
// probe a specific internal host (e.g. "imap.internal.example.com",
// "mcp.lan"). It matches the ORIGINAL hostname from the dial address, not
// the resolved IP — an allowlisted host bypasses the IP check entirely,
// by design (that's the point: the operator is vouching for that specific
// host being safe to reach even though it's internal).
type DialGuard struct {
	AllowedHosts []string

	// LoopbackHosts are hosts dialled only when the resolved address is a
	// loopback address (GitHub #494, integrations-hub-design "Amendment
	// 2026-10-21 (#494)"): a configured URL naming localhost reaches the local
	// machine, and nothing else, whatever the resolver says. Any other
	// resolved address, public or private, is refused. An AllowedHosts entry
	// for the same host wins (the operator vouches for it).
	LoopbackHosts []string

	// AllowPrivate admits loopback and private (RFC 1918, ULA) addresses
	// and refuses everything else isBlockedIP refuses: link-local (the
	// metadata service among them), 0.0.0.0/8, broadcast, multicast and
	// every embedded IPv4 form (GitHub #502, 18-security-and-tenancy.md
	// "Amendment 2026-10-22 (#502)", S3). It is for a probe an admin-class
	// caller points at a local or LAN model server; it never disables the
	// guard.
	AllowPrivate bool
}

// HTTPClient returns an *http.Client whose transport dials every connection
// through DialContext, bounded by timeout. Every prober constructor takes a
// client built this way — there is no un-guarded dial path.
//
// The transport never consults a proxy (GitHub #433/#434, review e89f F1):
// http.DefaultTransport carries Proxy: http.ProxyFromEnvironment, and with
// HTTP(S)_PROXY set the guarded dial would see the proxy's address, never the
// target's, so a request for 169.254.169.254 would be handed to the proxy
// unchecked. A guarded client dials its target directly or not at all.
func (g DialGuard) HTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = g.DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// NoRedirectHTTPClient is HTTPClient with redirects refused: a 3xx comes back
// to the caller as the response and no second request leaves the daemon
// (GitHub #433/#434, integrations-hub-design §0.0b). For a fetch where a
// redirect has no legitimate role, or whose request carries a header that must
// not reach a second host: Go strips only Authorization, Cookie and
// WWW-Authenticate on a cross-domain hop, never a custom header.
func (g DialGuard) NoRedirectHTTPClient(timeout time.Duration) *http.Client {
	c := g.HTTPClient(timeout)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// IsLoopbackHost reports whether host is the name localhost or a loopback IP
// literal (IPv4-mapped forms included). Only these literal forms count: a name
// that merely resolves to loopback ("localhost.", "x.localhost") does not.
func IsLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// LoopbackURLGuard is the guard for a configured server URL that may name a
// local service: its AllowedHosts are allowed, and the URL's own hostname,
// when that hostname is itself loopback (IsLoopbackHost), is a LoopbackHosts
// entry: dialled only when it resolves to loopback. Every other host must
// resolve to a public address. An unparsable URL gets allowed only. It is the
// one implementation of the rule an agent's MCP server (service.agentDialGuard)
// and the trading panel's broker fetch (GitHub #433) share.
//
// Amended 2026-10-21 (GitHub #494): the URL's host was an AllowedHosts entry,
// which skips the resolved-address check, so the NAME localhost was trusted
// wherever it resolved.
func LoopbackURLGuard(rawURL string, allowed []string) DialGuard {
	g := DialGuard{AllowedHosts: append([]string(nil), allowed...)}
	if u, err := url.Parse(rawURL); err == nil {
		if h := u.Hostname(); IsLoopbackHost(h) {
			g.LoopbackHosts = []string{h}
		}
	}
	return g
}

// DialContext is the guarded dial function used as http.Transport.
// DialContext. It resolves addr, checks the resolved IP (or the allowlist)
// via net.Dialer.Control, and either completes the connection or refuses
// it before any bytes are exchanged. Has the exact shape SMTPDialFunc
// (internal/email) expects, so a guard's method value can be passed
// straight through for the SMTP probe leg too.
func (g DialGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// addr without a port (shouldn't normally happen via http.Transport,
		// but DialContext is also called directly by the SMTP prober) —
		// fall back to treating the whole string as the host.
		host = addr
	}
	return g.Dialer(host).DialContext(ctx, network, addr)
}

// Dialer returns a *net.Dialer pre-configured to guard connections to
// host: allowlisted hosts get a plain dialer (opt-in bypass, by design);
// everything else gets a Control callback that inspects the *resolved* IP
// on every candidate address and refuses private/loopback/link-local ones.
//
// This is the seam email.IMAPDialConfig.Dialer expects — go-imap's
// imapclient.DialTLS takes a *net.Dialer via its Options (there is no
// DialFunc/net.Conn injection point on DialTLS itself), so the guard has
// to be expressible as a concrete *net.Dialer, not just a DialContext
// closure. DialContext (above) is built on top of this same method so the
// HTTP and IMAP paths share one guarding implementation.
func (g DialGuard) Dialer(host string) *net.Dialer {
	if g.hostAllowed(host) {
		// Allowlisted: skip the resolved-IP check entirely, per the type's
		// documented contract.
		return &net.Dialer{}
	}
	if g.loopbackOnly(host) {
		// GitHub #494: a loopback name reaches the local machine and nothing
		// else, whatever the resolver answers.
		return &net.Dialer{
			Control: func(_, address string, _ syscall.RawConn) error {
				ip := resolvedIP(address)
				if ip == nil {
					return fmt.Errorf("dial guard: could not parse resolved address %q", address)
				}
				if !ip.IsLoopback() {
					return fmt.Errorf("%w non-loopback address %s for loopback host %s", ErrDialRefused, ip, host)
				}
				return nil
			},
		}
	}
	return &net.Dialer{
		Control: func(_, address string, _ syscall.RawConn) error {
			ipHost, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil {
				ipHost = address
			}
			ip := net.ParseIP(ipHost)
			if ip == nil {
				return fmt.Errorf("dial guard: could not parse resolved address %q", address)
			}
			// GitHub #502: AllowPrivate admits a DIRECT loopback or private
			// address only; everything else isBlockedIP refuses stays refused.
			admitted := g.AllowPrivate && (ip.IsLoopback() || ip.IsPrivate())
			if isBlockedIP(ip) && !admitted {
				return fmt.Errorf("%w private/loopback/link-local address %s (host %s)", ErrDialRefused, ip, host)
			}
			return nil
		},
	}
}

// ErrDialRefused is the guard's refusal of a private, loopback or
// link-local address, so a caller can classify it without matching text
// (agent-administered design §6.1, review 5ed3 A4). The message is
// unchanged: "dial guard: refusing to dial private/loopback/... ".
var ErrDialRefused = errors.New("dial guard: refusing to dial")

// Allows reports whether host is allow-listed, so a dial to it skips the
// resolved-IP check. It is the dial's own match (exact string, no case or
// trailing-dot folding), exported so that a caller recording whether a dial
// bypassed the check (the MCP probe's audit row, GitHub #465) cannot drift
// from the dial.
func (g DialGuard) Allows(host string) bool { return g.hostAllowed(host) }

func (g DialGuard) loopbackOnly(host string) bool {
	for _, h := range g.LoopbackHosts {
		if h == host {
			return true
		}
	}
	return false
}

// resolvedIP parses the address a dialer's Control callback receives
// ("ip:port", or a bare IP).
func resolvedIP(address string) net.IP {
	ipHost, _, err := net.SplitHostPort(address)
	if err != nil {
		ipHost = address
	}
	return net.ParseIP(ipHost)
}

func (g DialGuard) hostAllowed(host string) bool {
	for _, h := range g.AllowedHosts {
		if h == host {
			return true
		}
	}
	return false
}

// isBlockedIP reports whether ip falls in a range DialGuard refuses by
// default: RFC1918 private, loopback, link-local (unicast or multicast),
// or unspecified (0.0.0.0 / ::).
//
// Amended 2026-10-21 (GitHub #434, review e89f F2/F4): also the rest of
// 0.0.0.0/8 ("this network"; Linux delivers 0.0.0.0 to the local host, so it
// is a loopback alias), the limited broadcast, every multicast scope, and an
// IPv6 address that embeds a blocked IPv4 address in the NAT64 well-known
// prefix 64:ff9b::/96, the deprecated IPv4-compatible ::/96 or 6to4
// 2002::/16 (code review fd9f F3). IPv4-mapped IPv6 (::ffff:a.b.c.d) is
// covered by the net.IP predicates, which test the embedded IPv4 address. The
// check is by address range: it does not model a site-specific NAT64 prefix.
func isBlockedIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil && (ip4[0] == 0 || ip4.Equal(net.IPv4bcast)) {
		return true
	}
	if len(ip) == net.IPv6len && ip.To4() == nil {
		switch {
		case nat64WellKnown.Contains(ip), ipv4Compatible.Contains(ip):
			// 64:ff9b::a.b.c.d and the deprecated ::a.b.c.d (code review
			// fd9f F3): the IPv4 address sits in the last four bytes. :: and
			// ::1 are in ::/96 too and are refused below either way.
			if isBlockedIP(ip[12:16]) {
				return true
			}
		case sixToFour.Contains(ip):
			// 2002:aabb:ccdd::/48 embeds the IPv4 address aa.bb.cc.dd.
			if isBlockedIP(ip[2:6]) {
				return true
			}
		}
	}
	return ip.IsPrivate() ||
		ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// The IPv6 prefixes that embed an IPv4 address isBlockedIP checks: RFC 6052's
// well-known NAT64 prefix, RFC 4291's deprecated IPv4-compatible form, and
// RFC 3056's 6to4.
var (
	nat64WellKnown = &net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)}
	ipv4Compatible = &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(96, 128)}
	sixToFour      = &net.IPNet{IP: net.ParseIP("2002::"), Mask: net.CIDRMask(16, 128)}
)

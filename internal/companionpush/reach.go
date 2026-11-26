// Package companionpush delivers companion completion and broker-action
// pushes — https://docs.vornik.io
// design.md §7 and §7a.
package companionpush

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// privateRanges are the only ranges a project's allowed_cidrs may open:
// RFC 1918, IPv6 unique-local, and the RFC 6598 shared range that overlay
// networks such as Tailscale use for LAN-style reach. They are what
// dialguard.IsGrantablePrivate admits; ParseAllowedCIDRs needs them as
// prefixes, and TestPrivateRangesMatchGrantablePrivate904 holds the two
// equal (GitHub #904).
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("100.64.0.0/10"),
}

// reachAllowed decides one resolved address. It is unmapped first, so an
// IPv4-mapped IPv6 address is judged as the IPv4 address it is.
//
// It is the daemon's one list, dialguard.IsBlockedIP, plus this guard's
// grant: allowed_cidrs may open a refused address only if it is grantable
// private and not a cloud metadata address. This is the
// DialGuard.PrivateHosts rule, which applies to names (GitHub #904;
// integrations-hub-design.md, "Change (2026-11-24): #904"). The list this
// guard kept before admitted 0.0.0.0/8, 255.255.255.255 and blocked IPv4
// addresses inside NAT64, 6to4 or IPv4-compatible IPv6, and let an opened
// range reach the metadata address inside it (100.100.100.200,
// fd00:ec2::254, fd20:ce::254).
func reachAllowed(a netip.Addr, allowed []netip.Prefix) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	ip := net.IP(a.AsSlice())
	if !dialguard.IsBlockedIP(ip) {
		return true
	}
	if !dialguard.IsGrantablePrivate(ip) || dialguard.IsCloudMetadata(ip) {
		return false
	}
	for _, p := range allowed {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ParseAllowedCIDRs parses a project's companion_push.allowed_cidrs. Each
// entry must lie wholly inside one of the private ranges; loopback,
// link-local, public and malformed entries are refused.
func ParseAllowedCIDRs(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, e := range entries {
		p, err := netip.ParsePrefix(strings.TrimSpace(e))
		if err != nil {
			return nil, fmt.Errorf("companion_push.allowed_cidrs: %q is not a CIDR", e)
		}
		p = p.Masked()
		if p.Addr().Is4In6() {
			return nil, fmt.Errorf("companion_push.allowed_cidrs: %q is IPv4-mapped; write it as IPv4", e)
		}
		inside := false
		for _, r := range privateRanges {
			if r.Bits() <= p.Bits() && r.Contains(p.Addr()) {
				inside = true
			}
		}
		if !inside {
			return nil, fmt.Errorf("companion_push.allowed_cidrs: %q must lie inside 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7 or 100.64.0.0/10", e)
		}
		out = append(out, p)
	}
	return out, nil
}

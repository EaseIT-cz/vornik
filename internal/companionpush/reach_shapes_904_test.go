package companionpush

import (
	"net"
	"net/netip"
	"testing"

	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/integrations/dialguard/dialguardtest"
)

// GitHub #904: companion push kept its own list. With no allowed_cidrs it
// admitted 0.0.0.0/8, 255.255.255.255 and blocked IPv4 addresses inside
// NAT64, 6to4 and IPv4-compatible IPv6; with a range opened it admitted the
// cloud metadata addresses inside it (100.100.100.200 in 100.64.0.0/10,
// fd00:ec2::254 and fd20:ce::254 in fc00::/7), which no DialGuard grant
// opens. Its only rule beyond the one list is its grant, allowed_cidrs.
func TestReachAllowedAgreesWithTheOneList904(t *testing.T) {
	all := mustPrefixes(t, "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7", "100.64.0.0/10")
	for _, s := range dialguardtest.Shapes {
		a := netip.MustParseAddr(s.Addr)
		if got := reachAllowed(a, nil); got != !s.Blocked {
			t.Errorf("reachAllowed(%s, none) = %v, the one list says blocked=%v", s.Addr, got, s.Blocked)
		}
		want := !s.Blocked || s.Grantable
		if got := reachAllowed(a, all); got != want {
			t.Errorf("reachAllowed(%s, every private range) = %v, want %v (blocked=%v grantable=%v)",
				s.Addr, got, want, s.Blocked, s.Grantable)
		}
	}
}

// GitHub #904: ParseAllowedCIDRs keeps privateRanges (it checks that a
// prefix lies inside a grantable range, which a per-address predicate cannot
// answer). Each range must be exactly what dialguard.IsGrantablePrivate
// admits: its first and last addresses grantable, its neighbours not.
func TestPrivateRangesMatchGrantablePrivate904(t *testing.T) {
	if len(privateRanges) != 5 {
		t.Fatalf("privateRanges has %d entries; extend this test with the new one", len(privateRanges))
	}
	for _, p := range privateRanges {
		first := p.Masked().Addr()
		last := lastAddr(p)
		for _, a := range []netip.Addr{first, last} {
			if !dialguard.IsGrantablePrivate(net.IP(a.AsSlice())) {
				t.Errorf("%s in %s is not grantable to dialguard", a, p)
			}
		}
		if prev := first.Prev(); prev.IsValid() && dialguard.IsGrantablePrivate(net.IP(prev.AsSlice())) {
			t.Errorf("%s, just below %s, is grantable to dialguard: the ranges differ", prev, p)
		}
		if next := last.Next(); next.IsValid() && dialguard.IsGrantablePrivate(net.IP(next.AsSlice())) {
			t.Errorf("%s, just above %s, is grantable to dialguard: the ranges differ", next, p)
		}
	}
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

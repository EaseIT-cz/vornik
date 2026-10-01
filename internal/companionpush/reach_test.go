package companionpush

import (
	"net/netip"
	"strings"
	"testing"
)

// Broker write-actions design §7 and §7a (reach): loopback, link-local,
// multicast and unspecified addresses are always refused; private ranges
// (RFC 1918, fc00::/7, 100.64.0.0/10) only when the task's project lists
// them; IPv4-mapped IPv6 is judged as the IPv4 address it is.
func TestReachAllowed(t *testing.T) {
	lan := mustPrefixes(t, "10.0.0.0/8", "fd00::/8", "100.64.0.0/10")
	cases := []struct {
		addr    string
		allowed []netip.Prefix
		want    bool
	}{
		{"93.184.216.34", nil, true},
		{"2606:4700::1111", nil, true},
		{"127.0.0.1", lan, false},
		{"::1", lan, false},
		{"::ffff:127.0.0.1", lan, false},
		{"169.254.1.1", lan, false},
		{"fe80::1", lan, false},
		{"0.0.0.0", lan, false},
		{"224.0.0.1", lan, false},
		{"10.0.0.5", nil, false},
		{"::ffff:10.0.0.5", nil, false},
		{"10.0.0.5", lan, true},
		{"::ffff:10.0.0.5", lan, true},
		{"192.168.1.10", lan, false},
		{"fd12:3456::1", lan, true},
		{"fd12:3456::1", nil, false},
		{"100.100.1.1", nil, false},
		{"100.100.1.1", lan, true},
	}
	for _, c := range cases {
		a := netip.MustParseAddr(c.addr)
		if got := reachAllowed(a, c.allowed); got != c.want {
			t.Errorf("reachAllowed(%s, %v) = %v, want %v", c.addr, c.allowed, got, c.want)
		}
	}
}

// §7a loader rule: an allowed_cidrs entry must lie wholly inside RFC 1918,
// fc00::/7 or 100.64.0.0/10.
func TestParseAllowedCIDRs(t *testing.T) {
	ok, err := ParseAllowedCIDRs([]string{"192.168.1.0/24", "fd00::/8", "100.64.0.0/10", "10.1.2.3/32"})
	if err != nil || len(ok) != 4 {
		t.Fatalf("valid entries refused: %v %v", ok, err)
	}
	for _, bad := range []string{"127.0.0.0/8", "169.254.0.0/16", "fe80::/10", "8.8.8.0/24",
		"0.0.0.0/0", "10.0.0.0/7", "::ffff:10.0.0.0/104", "not-a-cidr", "224.0.0.0/4"} {
		if _, err := ParseAllowedCIDRs([]string{bad}); err == nil || !strings.Contains(err.Error(), "allowed_cidrs") {
			t.Errorf("%q: want a refusal naming allowed_cidrs, got %v", bad, err)
		}
	}
}

func mustPrefixes(t *testing.T, s ...string) []netip.Prefix {
	t.Helper()
	p, err := ParseAllowedCIDRs(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

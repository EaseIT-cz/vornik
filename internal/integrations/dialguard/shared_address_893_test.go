package dialguard

import (
	"net"
	"testing"
)

// GitHub #893: IsSharedAddressSpace is RFC 6598 (100.64.0.0/10), IPv4 or
// IPv4-mapped, so the doctor finding agent_connections_cgnat asks the
// question in one place (agent-administered design part 06 §6.11). Since
// option A shipped, isBlockedIP and the dial grants call it too.
func TestIsSharedAddressSpace_893(t *testing.T) {
	cases := map[string]bool{
		"100.64.0.0":        true,
		"100.100.1.2":       true,
		"100.127.255.255":   true,
		"::ffff:100.64.0.1": true,
		"100.63.255.255":    false,
		"100.128.0.0":       false,
		"10.0.0.1":          false,
		"8.8.8.8":           false,
		"fd00::1":           false,
		"2606:4700::1111":   false,
	}
	for s, want := range cases {
		if got := IsSharedAddressSpace(net.ParseIP(s)); got != want {
			t.Errorf("IsSharedAddressSpace(%s) = %v, want %v", s, got, want)
		}
	}
	if IsSharedAddressSpace(nil) {
		t.Error("IsSharedAddressSpace(nil) = true, want false")
	}
	// Since #893 option A the one list refuses the whole range, the
	// metadata address with it (cgnat_private_893_test.go).
	if !IsBlockedIP(net.ParseIP("100.101.102.103")) {
		t.Error("IsBlockedIP(100.101.102.103) = false; CGNAT is private since #893")
	}
	if !IsBlockedIP(net.ParseIP("100.100.100.200")) {
		t.Error("IsBlockedIP(100.100.100.200) = false; the metadata address must stay refused")
	}
}

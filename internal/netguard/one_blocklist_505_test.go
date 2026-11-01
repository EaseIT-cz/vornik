package netguard

import (
	"net"
	"testing"
)

// GitHub #505 (automated security review of a708bd96a): netguard kept its own
// list of refused addresses beside dialguard's, and it missed what dialguard
// refuses: Alibaba Cloud's metadata address, the IPv6 cloud-metadata
// addresses' embedded IPv4 cousins, 0.0.0.0/8, broadcast, and IPv4 embedded
// in NAT64 / 6to4 / IPv4-compatible IPv6. One list now decides for both.
// Found at: review, observed at integration (the URL-liveness checker
// dialling a URL from ingested content).
func TestIsPrivateIP505_SameListAsDialGuard(t *testing.T) {
	for _, a := range []string{
		"100.100.100.200",    // Alibaba Cloud metadata
		"64:ff9b::a9fe:a9fe", // NAT64 embedding of 169.254.169.254
		"2002:a9fe:a9fe::",   // 6to4 embedding of 169.254.169.254
		"64:ff9b::6464:64c8", // NAT64 embedding of 100.100.100.200
		"0.1.2.3",            // 0.0.0.0/8
		"255.255.255.255",    // broadcast
		"fd00:ec2::254",      // AWS IMDS IPv6 (ULA: refused here already)
		"169.254.169.254",
		"::a9fe:a9fe", // IPv4-compatible embedding of 169.254.169.254
	} {
		if !IsPrivateIP(net.ParseIP(a)) {
			t.Errorf("IsPrivateIP(%s) = false: netguard would dial it", a)
		}
	}
	if IsPrivateIP(net.ParseIP("198.51.100.7")) {
		t.Error("a public address is refused")
	}
}

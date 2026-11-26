package urlguard

import (
	"net"
	"testing"

	"vornik.io/vornik/internal/integrations/dialguard/dialguardtest"
)

// writeTargetOnly904 are the shapes the web guards refuse beyond the one
// list, by their named rule (writeTargetBlockedNets): documentation,
// benchmarking, reserved, discard and site-local ranges, and whole IPv6
// translation prefixes (the one list refuses only the blocked IPv4 addresses
// inside NAT64, 6to4 and IPv4-compatible forms).
var writeTargetOnly904 = map[string]bool{
	"192.0.0.1": true, "192.0.2.1": true, "192.88.99.1": true, "198.18.0.1": true,
	"198.19.255.255": true, "198.51.100.1": true, "203.0.113.1": true, "240.0.0.1": true,
	"255.255.255.254": true,
	"::808:808":       true, "64:ff9b::808:808": true, "64:ff9b:1::a9fe:a9fe": true,
	"2002:808:808::": true, "2001::1": true, "2001:db8::1": true, "100::1": true,
	"fec0::1": true,
}

// GitHub #904: web/urlguard kept its own CIDR table and admitted
// ::a9fe:a9fe (169.254.169.254 in the IPv4-compatible form), which the one
// list refuses. It must now agree with the one list on every shape, except
// the shapes its named table refuses on top.
func TestIsBlockedIPAgreesWithTheOneList904(t *testing.T) {
	for _, s := range dialguardtest.Shapes {
		ip := net.ParseIP(s.Addr)
		want := s.Blocked || writeTargetOnly904[s.Addr]
		if got := isBlockedIP(ip); got != want {
			t.Errorf("isBlockedIP(%s) = %v, want %v (one list %v, write-target rule %v)",
				s.Addr, got, want, s.Blocked, writeTargetOnly904[s.Addr])
		}
		if writeTargetOnly904[s.Addr] && s.Blocked {
			t.Errorf("%s is listed as write-target only but the one list refuses it", s.Addr)
		}
	}
}

// GitHub #904: the scraper has only the table (it cannot call the Go
// predicate), and the parity test holds its table equal to this one. So the
// table alone must refuse every shape the one list refuses.
func TestWriteTargetTableCoversTheOneList904(t *testing.T) {
	for _, s := range dialguardtest.Shapes {
		if s.Blocked && !inWriteTargetTable(net.ParseIP(s.Addr)) {
			t.Errorf("the table admits %s, which the one list refuses; the scraper would admit it", s.Addr)
		}
	}
}

// GitHub #904: nil is refused, as netguard.IsPrivateIP refuses it.
func TestIsBlockedIPRefusesNil904(t *testing.T) {
	if !isBlockedIP(nil) {
		t.Error("isBlockedIP(nil) = false, want true")
	}
}

package dialguard

import (
	"net"
	"testing"

	"vornik.io/vornik/internal/integrations/dialguard/dialguardtest"
)

// GitHub #904: every address guard is tested against dialguardtest.Shapes.
// This test anchors the table to the one list, so a guard test that agrees
// with the table agrees with isBlockedIP and the private grant rule.
func TestShapesMatchTheOneList904(t *testing.T) {
	for _, s := range dialguardtest.Shapes {
		ip := net.ParseIP(s.Addr)
		if ip == nil {
			t.Fatalf("shape %q does not parse", s.Addr)
		}
		if got := IsBlockedIP(ip); got != s.Blocked {
			t.Errorf("IsBlockedIP(%s) = %v, table says %v", s.Addr, got, s.Blocked)
		}
		if got := IsGrantablePrivate(ip) && !IsCloudMetadata(ip); got != s.Grantable {
			t.Errorf("grantable(%s) = %v, table says %v", s.Addr, got, s.Grantable)
		}
		if s.Grantable && !s.Blocked {
			t.Errorf("shape %s is grantable but not blocked: a grant opens only what the list refuses", s.Addr)
		}
	}
}

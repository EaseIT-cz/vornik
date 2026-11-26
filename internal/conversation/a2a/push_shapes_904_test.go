package a2a

import (
	"net"
	"testing"

	"vornik.io/vornik/internal/integrations/dialguard/dialguardtest"
)

// GitHub #904: the A2A webhook guard kept its own list and admitted
// 0.0.0.0/8, 255.255.255.255 and a blocked IPv4 address inside NAT64, 6to4
// or IPv4-compatible IPv6 (64:ff9b::a9fe:a9fe is 169.254.169.254), all of
// which the daemon's one list refuses. It has no grant and no extra rule, so
// it must agree with the one list on every shape.
func TestIsBlockedPushIPAgreesWithTheOneList904(t *testing.T) {
	for _, s := range dialguardtest.Shapes {
		if got := isBlockedPushIP(net.ParseIP(s.Addr)); got != s.Blocked {
			t.Errorf("isBlockedPushIP(%s) = %v, the one list says %v", s.Addr, got, s.Blocked)
		}
	}
}

// GitHub #904: the URL check uses the same predicate, so a literal
// NAT64-wrapped metadata address is refused at submit time too.
func TestValidateWebhookURLRefusesEmbeddedMetadata904(t *testing.T) {
	for _, raw := range []string{
		"https://[64:ff9b::a9fe:a9fe]/hook",
		"https://[2002:a9fe:a9fe::]/hook",
		"https://0.1.2.3/hook",
	} {
		if err := ValidateWebhookURL(raw); err == nil {
			t.Errorf("ValidateWebhookURL(%s) = nil, want a refusal", raw)
		}
	}
}

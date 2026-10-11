package realip

import "testing"

// GitHub #103 (2026-10-07, T15 review a748; defect found in review 5e1a,
// 2026-10-08): NewConfig widened a bare IP by its parsed form, so the
// IPv4-mapped IPv6 literal ::ffff:192.0.2.1 got "/32" and was parsed as the
// IPv6 network ::/32. That entry trusted [::1] and not the named host. The
// suffix is now chosen by the literal's syntax: a literal with ':' gets /128.
func TestNewConfig_BareMappedIPv6IsOneHost(t *testing.T) {
	c := mustConfig(t, true, []string{"::ffff:192.0.2.1"}, "CF-Connecting-IP")

	r := req("192.0.2.1:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r); got != "203.0.113.7" {
		t.Fatalf("the named host must be trusted: want 203.0.113.7, got %q", got)
	}
	r = req("[::1]:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	if got := c.ResolveClientIP(r); got != "::1" {
		t.Fatalf("[::1] must not be trusted by a bare mapped entry, got %q", got)
	}
	ones, bits := c.TrustedProxies[0].Mask.Size()
	if ones != bits {
		t.Fatalf("bare entry must be a single address: mask %d/%d", ones, bits)
	}
}

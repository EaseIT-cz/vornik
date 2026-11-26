package config

import "testing"

// GitHub #168 (integrations-hub-design, "Amendment 2026-11-24 (#168)", site
// 2): the gateway.address check compared "localhost" case-sensitively, so
// LOCALHOST, which reaches the same machine, was refused at boot. It now
// calls dialguard.IsLoopbackHost. Old: error. New: accepted. A non-loopback
// name that merely contains localhost stays refused.
func TestGateway_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	for addr, ok := range map[string]bool{
		"http://LOCALHOST:8010":          true,
		"http://127.0.0.2:8010":          true,
		"http://[::ffff:127.0.0.1]:8010": true,
		"http://localhost.:8010":         false,
		"http://localhost.evil.com:8010": false,
		"http://127.0.0.1.nip.io:8010":   false,
		"http://0.0.0.0:8010":            false,
	} {
		c := gatewayValidBase()
		c.Gateway.Address = addr
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("gateway.address %q: Validate = %v, want ok=%v", addr, err, ok)
		}
	}
}

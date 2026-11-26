package agentadmin

import "testing"

// GitHub #907 (review-20261124-bb28 F8): an IP model endpoint counted as local
// whenever the blocklist held it, but the dial admits only loopback and a
// grantable private address. A NAT64, IPv4-compatible or 6to4 embedding of
// 10.0.0.5, or a link-local address, was "local" (no device approval) and
// then refused at every dial. Locality now follows what the dial admits.
func TestDestinationOf_907_LocalityFollowsTheDial(t *testing.T) {
	cases := map[string]bool{
		"https://[64:ff9b::a00:5]/v1":    false, // NAT64 of 10.0.0.5
		"https://[::a00:5]/v1":           false, // IPv4-compatible
		"https://[2002:a00:5::]/v1":      false, // 6to4
		"https://169.254.10.1/v1":        false, // link-local
		"https://[fe80::1]/v1":           false,
		"https://169.254.169.254/v1":     false, // cloud metadata
		"https://100.100.100.200/v1":     false,
		"https://100.64.0.1/v1":          false, // CGNAT stays remote (#893)
		"https://[::ffff:100.64.0.1]/v1": false,
		"http://127.0.0.1:11434/v1":      true,
		"http://[::1]:11434/v1":          true,
		"http://localhost:11434/v1":      true,
		"https://10.0.0.5:11434/v1":      true,
		"https://[::ffff:10.0.0.5]/v1":   true,
		"https://192.168.1.20/v1":        true,
		"https://[fd00::1]/v1":           true,
		"https://198.51.100.7/v1":        false,
	}
	for ep, want := range cases {
		d, why := DestinationOf(ModelRoute{SubProvider: "http", Endpoint: ep}, true)
		if why != "" {
			t.Fatalf("%s: refused: %s", ep, why)
		}
		if d.Local != want {
			t.Errorf("%s: Local = %v, want %v", ep, d.Local, want)
		}
	}
}

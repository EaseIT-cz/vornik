package agentadmin

import "testing"

// GitHub #893 option A (agent-administered design part 06 §6.11, "Change
// (2026-11-24): #893 option A shipped"): the dial guard's list now refuses
// CGNAT 100.64.0.0/10, so netguard.IsPrivateIP holds for it. Model locality
// does not follow: a model served from a tailnet peer stays remote and keeps
// its per-destination device approval. Loopback, RFC 1918 and ULA stay local.
func TestDestinationOf_CGNATStaysRemote_893(t *testing.T) {
	cases := map[string]bool{
		"https://100.64.0.1/v1":          false,
		"https://100.101.102.103:11434/": false,
		"https://100.127.255.254/v1":     false,
		"https://100.100.100.200/v1":     false,
		"https://[::ffff:100.64.0.1]/v1": false,
		// Review 0a2f F2: the embedded forms the dial refuses are not local.
		"https://[64:ff9b::6440:1]/v1": false,
		"https://[::6440:1]/v1":        false,
		"https://[2002:6440:1::]/v1":   false,
		"https://[64:ff9b::a00:5]/v1":  false, // NAT64 of 10.0.0.5: the dial refuses it (#907)
		"https://10.0.0.5:11434/v1":    true,
		"https://192.168.1.20/v1":      true,
		"https://[fd00::1]/v1":         true,
		"http://127.0.0.1:11434/v1":    true,
		"http://localhost:11434/v1":    true,
		"https://198.51.100.7/v1":      false,
	}
	for ep, wantLocal := range cases {
		d, why := DestinationOf(ModelRoute{SubProvider: "http", Endpoint: ep}, true)
		if why != "" {
			t.Fatalf("%s: refused: %s", ep, why)
		}
		if d.Local != wantLocal {
			t.Errorf("%s: Local = %v, want %v", ep, d.Local, wantLocal)
		}
	}
}

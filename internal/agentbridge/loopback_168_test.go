package agentbridge

import "testing"

// GitHub #168 (integrations-hub-design, "Amendment 2026-11-24 (#168)", site
// 8; review 4dca item 1): the bridge posts the bearer key with a plain
// http.Client, which never checks where a name resolves. So CheckEndpoint
// takes dialguard.IsLoopbackHostForPlainDial: a loopback IP literal or
// exactly "localhost", as before. LOCALHOST stays refused (no new name
// spelling on an unguarded dial); other loopback literals are now accepted
// (dialled as themselves, no resolution). Near-misses stay refused, so the
// key is never sent in cleartext to another host.
func TestCheckEndpoint_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://localhost:8080":          true,
		"http://127.0.0.2:8080":          true,
		"http://[::ffff:127.0.0.1]:8080": true,
		"http://LOCALHOST:8080":          false,
		"http://localhost.:8080":         false,
		"http://localhost.evil.com:8080": false,
		"http://127.0.0.1.nip.io":        false,
		"http://0.0.0.0:8080":            false,
	} {
		if err := CheckEndpoint(raw); (err == nil) != ok {
			t.Errorf("CheckEndpoint(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}

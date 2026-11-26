package telemetryclient

import "testing"

// GitHub #168 (integrations-hub-design, "Amendment 2026-11-24 (#168)", site
// 14; review 4dca item 1): BuildRequest allowed plain http only to
// "127.0.0.1" and "localhost". The client is a plain http.Client, which
// never checks where a name resolves, so it takes
// dialguard.IsLoopbackHostForPlainDial: any loopback IP literal (dialled as
// itself) or exactly "localhost". Old: "must use https" for [::1] and
// 127.0.0.2. New: accepted. LOCALHOST stays refused, as before.
func TestBuildRequest_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	ev := InstallEvent("2026.7.4", SourceQuickstart)
	for endpoint, ok := range map[string]bool{
		"http://localhost:9000/e":        true,
		"http://[::1]:9000/e":            true,
		"http://127.0.0.2:9000/e":        true,
		"http://LOCALHOST:9000/e":        false,
		"http://localhost.:9000/e":       false,
		"http://127.0.0.1.nip.io:9000/e": false,
		"http://telemetry.example/e":     false,
	} {
		if _, err := BuildRequest(endpoint, ev); (err == nil) != ok {
			t.Errorf("BuildRequest(%q) = %v, want ok=%v", endpoint, err, ok)
		}
	}
}

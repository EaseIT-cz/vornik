package registry

import "testing"

// GitHub #168 (integrations-hub-design, "Amendment 2026-11-24 (#168)", site
// 9): checkAPIBaseURL compared "localhost" case-sensitively, so an http
// base_url on LOCALHOST was refused though it reaches this machine. It now
// calls dialguard.IsLoopbackHost. Old: refused. New: accepted. Near-misses
// stay refused.
func TestCheckAPIBaseURL_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://LOCALHOST:8080":          true,
		"http://127.0.0.2:8080":          true,
		"http://[::1]:8080":              true,
		"http://localhost.:8080":         false,
		"http://localhost.evil.com:8080": false,
		"http://127.0.0.1.nip.io":        false,
		"http://0.0.0.0:8080":            false,
	} {
		if err := checkAPIBaseURL(raw); (err == nil) != ok {
			t.Errorf("checkAPIBaseURL(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}

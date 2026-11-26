package mcpconnect

import (
	"errors"
	"testing"
)

// GitHub #168 (integrations-hub-design, "Amendment 2026-11-24 (#168)", site
// 3): isLoopback accepted only "localhost", "127.0.0.1" and "::1", so an
// http public_base_url on another loopback literal or LOCALHOST was refused
// as "neither https nor loopback". It now calls dialguard.IsLoopbackHost.
// Old: ErrNoPublicBaseURL. New: a redirect URI. Near-misses stay refused.
func TestRedirectURI_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	for base, ok := range map[string]bool{
		"http://127.0.0.2:8080":          true,
		"http://[::ffff:127.0.0.1]:8080": true,
		"http://LOCALHOST:8080":          true,
		"http://localhost.:8080":         false,
		"http://localhost.evil.com":      false,
		"http://127.0.0.1.nip.io":        false,
		"http://0.0.0.0:8080":            false,
		"http://[::]:8080":               false,
	} {
		c := newConnector(t, newFakeTokens(), nil, base)
		_, err := c.RedirectURI()
		if ok && err != nil {
			t.Errorf("base %q: RedirectURI = %v, want a URI", base, err)
		}
		if !ok && !errors.Is(err, ErrNoPublicBaseURL) {
			t.Errorf("base %q: RedirectURI err = %v, want ErrNoPublicBaseURL", base, err)
		}
	}
}

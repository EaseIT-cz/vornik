package instanceid

import "testing"

func TestPattern566(t *testing.T) {
	for _, ok := range []string{"a", "acme", "inst-42", "a1-b2"} {
		if !Pattern.MatchString(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "ac.me", "Acme", "-a", "a-", "a_b", "a/b"} {
		if Pattern.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

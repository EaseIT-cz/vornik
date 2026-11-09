package apikey

import "testing"

// GitHub #445: SecretEqual compares fixed-length digests, so its timing does
// not depend on the stored secret's length, and an empty side never matches.
func TestSecretEqual_445(t *testing.T) {
	for _, c := range []struct {
		presented, stored string
		want              bool
	}{
		{"s3cr3t-token", "s3cr3t-token", true},
		{"s3cr3t-tokeN", "s3cr3t-token", false},
		{"s3cr3t", "s3cr3t-token", false},
		{"s3cr3t-token-longer", "s3cr3t-token", false},
		{"", "s3cr3t-token", false},
		{"s3cr3t-token", "", false},
		{"", "", false},
	} {
		if got := SecretEqual(c.presented, c.stored); got != c.want {
			t.Errorf("SecretEqual(%q, %q) = %v, want %v", c.presented, c.stored, got, c.want)
		}
	}
}

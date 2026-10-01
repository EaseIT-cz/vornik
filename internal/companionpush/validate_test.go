package companionpush

import (
	"net/netip"
	"strings"
	"testing"
)

// Design §7a: the companion notify URL check accepts a literal private
// address only inside the project's allowed_cidrs. Hostnames pass here and
// are judged at connect time on their resolved address.
func TestValidateNotifyURL(t *testing.T) {
	lan := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
	for _, c := range []struct {
		url     string
		allowed []netip.Prefix
		ok      bool
	}{
		{"https://hooks.example.com/vornik", nil, true},
		{"http://192.168.1.20:8080/push", lan, true},
		{"http://192.168.1.20:8080/push", nil, false},
		{"http://192.168.2.20/push", lan, false},
		{"http://127.0.0.1/push", lan, false},
		{"http://[::1]/push", lan, false},
		{"http://localhost/push", lan, false},
		{"http://LOCALHOST./push", lan, false},
		{"http://169.254.169.254/latest", lan, false},
		{"ftp://hooks.example.com/", nil, false},
		{"not a url", nil, false},
		{"", nil, false},
	} {
		err := ValidateNotifyURL(c.url, c.allowed)
		if (err == nil) != c.ok {
			t.Errorf("ValidateNotifyURL(%q, %v) = %v, want ok=%v", c.url, c.allowed, err, c.ok)
		}
	}
}

func TestValidateNotifyToken(t *testing.T) {
	for _, bad := range []string{strings.Repeat("a", 513), "tok\nen", "tok\x00", "tok\x7f"} {
		if err := ValidateNotifyToken(bad); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
	for _, good := range []string{"", "s3cr3t-token_value.123", strings.Repeat("a", 512)} {
		if err := ValidateNotifyToken(good); err != nil {
			t.Errorf("token %q refused: %v", good, err)
		}
	}
}

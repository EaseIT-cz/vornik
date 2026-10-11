package mcpauth

import (
	"net/url"
	"testing"
)

// GitHub #96 (2026-10-08), review c820 finding 4: the gateway client keeps
// its own copy of this origin rule (internal/apigateway/gateway sameOrigin);
// the same rows run against both so the two stay in step.
func TestSameURLOrigin_Rows(t *testing.T) {
	rows := []struct {
		a, b string
		same bool
	}{
		{"http://h", "http://h:80/x", true},
		{"https://h", "https://h:443/x", true},
		{"HTTP://H:8000", "http://h:8000/x", true},
		{"http://[::1]:8000", "http://[::1]:8000/x", true},
		{"http://[::1]:8000", "http://[::1]:8001/x", false},
		{"http://h:8000", "http://h:8001/x", false},
		{"http://h:8000", "https://h:8000/x", false},
		{"http://h:8000", "http://g:8000/x", false},
	}
	for _, r := range rows {
		a, _ := url.Parse(r.a)
		b, _ := url.Parse(r.b)
		if got := sameURLOrigin(a, b); got != r.same {
			t.Errorf("sameURLOrigin(%s, %s) = %v, want %v", r.a, r.b, got, r.same)
		}
	}
}

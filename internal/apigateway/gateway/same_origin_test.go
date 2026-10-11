package gateway

import (
	"net/url"
	"testing"
)

// GitHub #96 (2026-10-08), review c820: default ports, case and IPv6
// literals, for the origin rule the gateway redirect check uses.
// sameOriginRows are shared in spirit with mcpauth's TestSameURLOrigin_Rows
// (the same cases run against its helper), so the two rules stay in step.
var sameOriginRows = []struct {
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

func TestSameOrigin(t *testing.T) {
	for _, r := range sameOriginRows {
		a, _ := url.Parse(r.a)
		b, _ := url.Parse(r.b)
		if got := sameOrigin(a, b); got != r.same {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", r.a, r.b, got, r.same)
		}
	}
}

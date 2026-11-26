package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/integrations/dialguard"
)

// GitHub #168, review 4dca item 1 (integrations-hub-design, "Amendment
// 2026-11-24 (#168)"): config now admits gateway.address on LOCALHOST, so the
// gateway client must check where the name resolves. It put the URL's host
// in AllowedHosts, which dials with no address check (the #494 gap, for
// lowercase localhost too). It now dials through dialguard.LoopbackURLGuard:
// the gateway host is reached only at a loopback address.
//
// Lower than a resolver-level test because a test cannot make a name resolve
// elsewhere without editing /etc/hosts (as #494's tests say); a private
// literal shows which guard the client holds. Old: AllowedHosts, so the
// client dialled 10.0.0.5 unchecked. New: the dial is refused before connect.
func TestNew_GatewayHostIsDialledOnlyAtLoopback_168(t *testing.T) {
	c, err := New("http://10.0.0.5:8010", "tok", apigateway.Registry{}, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://10.0.0.5:8010/", nil)
	_, err = c.httpc.Do(req)
	if !errors.Is(err, dialguard.ErrDialRefused) {
		t.Fatalf("dial of a non-loopback gateway host: err = %v, want ErrDialRefused", err)
	}

	// The real shape still works: a loopback gateway, named in any case.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	for _, base := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), strings.Replace(srv.URL, "127.0.0.1", "LOCALHOST", 1)} {
		c, err := New(base, "tok", apigateway.Registry{}, 5*time.Second)
		if err != nil {
			t.Fatalf("New(%q): %v", base, err)
		}
		resp, err := c.httpc.Get(base + "/")
		if err != nil {
			t.Fatalf("GET %s: %v", base, err)
		}
		_ = resp.Body.Close()
	}
}

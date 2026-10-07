package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Regression: custom SSRF redirect callbacks accidentally removed Go's default request bound.
func TestPublicRedirectRequestBound(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hops      int
		loop      bool
		wantCalls int
		wantError bool
		private   bool
	}{
		{"legitimate", 2, false, 3, false, false}, {"boundary", 9, false, 10, false, false}, {"too-long", 10, false, 10, true, false}, {"cycle", 12, true, 10, true, false},
		{"private-first", 0, false, 1, true, true},
		{"private-sixth", 5, false, 6, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				step, _ := strconv.Atoi(r.URL.Query().Get("step"))
				if tc.private && step == tc.hops {
					http.Redirect(w, r, "http://127.0.0.1/private", http.StatusFound)
					return
				}
				if n < 13 && (tc.loop || step < tc.hops) {
					http.Redirect(w, r, fmt.Sprintf("/?step=%d", step+1), http.StatusFound)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			client := NewGuardedClient(2 * time.Second)
			transport := client.Transport.(*http.Transport).Clone()
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			client.Transport = transport
			defer transport.CloseIdleConnections()
			response, err := client.Get("http://8.8.8.8/?step=0")
			if response != nil {
				_ = response.Body.Close()
			}
			if (err != nil) != tc.wantError || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("redirect boundary: calls=%d err=%v; want calls=%d error=%v", calls.Load(), err, tc.wantCalls, tc.wantError)
			}
		})
	}
}

package autonomy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// GitHub #465 (2026-10-21, trading-dashboard-design "Amendment 2026-10-21
// (#465)"): brokerReachable fetched /caps with http.DefaultClient, following
// any redirect with no dial guard. It now dials through the shared guard with
// redirects refused and reads through integrations.ReadBoundedBody. Real HTTP
// broker (httptest), the level the broker's answer reaches the precheck.

func cleanCapsServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"configured":{"mode":"paper"},"portfolio":{"account":"DU-X"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestBrokerReachable_RedirectIsNotFollowed_Issue465(t *testing.T) {
	target, hits := cleanCapsServer(t)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/caps", http.StatusFound)
	}))
	defer broker.Close()

	if brokerReachable(context.Background(), broker.URL) {
		t.Error("a redirecting /caps must read as not reachable")
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target received %d request(s); want none", hits.Load())
	}
}

func TestBrokerReachable_RebindingShapedHostIsRefused_Issue465(t *testing.T) {
	requireLoopbackAlias465(t)
	backend, hits := cleanCapsServer(t)
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost.:" + u.Port()

	if brokerReachable(context.Background(), u.String()) {
		t.Error("a name resolving to loopback (not the literal) must be refused")
	}
	if hits.Load() != 0 {
		t.Fatalf("dialled %d time(s); want none", hits.Load())
	}
}

func TestBrokerReachable_OversizedBodyStaysLenient_Issue465(t *testing.T) {
	// The lenient rule is unchanged: an unreadable or unparsable 200 body is
	// reachable. Before #465 an oversized body was truncated and failed to
	// parse (reachable); now it fails the bounded read (reachable). Same answer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"portfolio_error":"`+strings.Repeat("x", 64*1024)+`"}`)
	}))
	defer srv.Close()
	if !brokerReachable(context.Background(), srv.URL) {
		t.Error("an oversized 200 body must stay reachable (lenient rule)")
	}
}

// requireLoopbackAlias465 pins the rebinding tests' premise (code review a71e
// finding 1): "localhost." must resolve, and only to loopback, so that the
// dial reaches the guard's resolved-IP check. Without it a DNS failure would
// refuse the dial for the wrong reason and the test could pass on broken code.
func requireLoopbackAlias465(t *testing.T) {
	t.Helper()
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost.")
	if err != nil || len(addrs) == 0 {
		t.Fatalf("test premise: \"localhost.\" must resolve on this host (got %v, %v)", addrs, err)
	}
	for _, a := range addrs {
		if !a.IP.IsLoopback() {
			t.Fatalf("test premise: \"localhost.\" resolved to non-loopback %s", a.IP)
		}
	}
}

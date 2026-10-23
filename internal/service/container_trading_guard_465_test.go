package service

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
// (#465)"): fetchBrokerOrdersByTag (the boot reconcile of stale submitted
// orders) posted get_orders with http.DefaultClient, following any redirect
// with no dial guard. It now dials through the shared guard with redirects
// refused and reads through integrations.ReadBoundedBody. Real HTTP broker.

func countingOKServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetchBrokerOrdersByTag_RedirectIsNotFollowed_Issue465(t *testing.T) {
	target, hits := countingOKServer(t)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/message", http.StatusTemporaryRedirect)
	}))
	defer broker.Close()

	_, err := fetchBrokerOrdersByTag(context.Background(), broker.URL)
	if err == nil || !strings.Contains(err.Error(), "broker returned 307") {
		t.Fatalf("want broker returned 307, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target received %d request(s); want none", hits.Load())
	}
}

func TestFetchBrokerOrdersByTag_RebindingShapedHostIsRefused_Issue465(t *testing.T) {
	requireLoopbackAlias465(t)
	backend, hits := countingOKServer(t)
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "localhost.:" + u.Port()

	if _, err := fetchBrokerOrdersByTag(context.Background(), u.String()); err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Fatalf("want a dial guard refusal, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("dialled %d time(s); want none", hits.Load())
	}
}

func TestFetchBrokerOrdersByTag_OversizedBodyIsRefusedNotTruncated_Issue465(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", 1024*1024+1))
	}))
	defer srv.Close()
	_, err := fetchBrokerOrdersByTag(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want a size refusal, got %v", err)
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

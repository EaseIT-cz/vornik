package approverdevice

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// BACKLOG 2026-10-05 (operator report), T12: plain-http pairing to a non-loopback host
func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "localhost:8080": true, "LOCALHOST.": true, "localhost.:8080": true,
		"127.0.0.1": true, "127.0.0.1:80": true, "127.5.5.5:8080": true,
		"[::1]": true, "[::ffff:127.0.0.1]:8080": true, "[::1]:8080": true, "::1": true,
		"192.168.0.142:8080": false, "example.com": false, "localhost.evil.com": false,
		"127.0.0.1.evil.com": false, "": false, "[fe80::1]:80": false, "foo.localhost": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestHTTPSRefusalMetrics_RecordAndNilSafe(t *testing.T) {
	var nilm *HTTPSRefusalMetrics
	nilm.Attach(nil)
	nilm.Record("pair") // must not panic
	m := NewHTTPSRefusalMetrics()
	m.Record("pair") // unattached: no-op
	reg := prometheus.NewRegistry()
	m.Attach(reg)
	m.Attach(reg) // second attach must not re-register
	m.Record("pair")
	m.Record("approve")
	m.Record("pair")
	if got := testutil.ToFloat64(m.total.WithLabelValues("pair")); got != 2 {
		t.Fatalf("pair = %v, want 2", got)
	}
}

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// brokerHealthServer serves a broker /health body.
func brokerHealthServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Design: trading-broker-design.md, amendment 2026-10-23 change (3).
func TestCheckBrokerAuditChannel_Unwired(t *testing.T) {
	h := NewDoctorHandlers(nil)
	if c := h.checkBrokerAuditChannel(context.Background()); c.Status != "SKIPPED" {
		t.Fatalf("unwired: %+v", c)
	}
	h.SetBrokerTargets(func() []BrokerTarget { return nil })
	if c := h.checkBrokerAuditChannel(context.Background()); c.Status != "SKIPPED" {
		t.Fatalf("no broker projects: %+v", c)
	}
}

// Incident 2026-10-01..23: broker-mcp ran 22 days with a key the daemon
// rejected; its healthcheck said so and nothing in Vornik reported it, so the
// 2026-10-13 GOOGL round trip vanished from the ledger unnoticed. Found at
// production; regression test at integration (the check against a real HTTP
// /health) — lower because the broker's side of the signal is already pinned
// by TestHealth_ReportsAuditBacklog and the daemon side is this read alone.
func TestCheckBrokerAuditChannel_AuthRejectedIsError_Incident20261001(t *testing.T) {
	url := brokerHealthServer(t, `{"ok":true,"broker_available":true,"audit_channel_auth_ok":false,"audit_pending":9}`)
	h := NewDoctorHandlers(nil)
	h.SetBrokerTargets(func() []BrokerTarget { return []BrokerTarget{{ProjectID: "ibkr-trader", BaseURL: url}} })
	c := h.checkBrokerAuditChannel(context.Background())
	if c.Status != "ERROR" {
		t.Fatalf("want ERROR, got %+v", c)
	}
	joined := strings.Join(c.Items, "\n")
	for _, want := range []string{"ibkr-trader", "VORNIK_BROKER_DAEMON_API_KEY", "recreate"} {
		if !strings.Contains(joined, want) {
			t.Errorf("items missing %q: %s", want, joined)
		}
	}
	if !strings.Contains(c.Message, "1 broker(s) examined") {
		t.Errorf("message must carry the denominator: %s", c.Message)
	}
}

func TestCheckBrokerAuditChannel_BacklogIsWarning(t *testing.T) {
	url := brokerHealthServer(t, `{"ok":true,"audit_channel_auth_ok":true,"audit_pending":3}`)
	h := NewDoctorHandlers(nil)
	h.SetBrokerTargets(func() []BrokerTarget { return []BrokerTarget{{ProjectID: "p", BaseURL: url}} })
	if c := h.checkBrokerAuditChannel(context.Background()); c.Status != "WARNING" || !strings.Contains(strings.Join(c.Items, ""), "3") {
		t.Fatalf("want WARNING naming the backlog, got %+v", c)
	}
}

// Unreachable is "not examined", never OK.
func TestCheckBrokerAuditChannel_UnreachableIsWarningNotOK(t *testing.T) {
	h := NewDoctorHandlers(nil)
	h.SetBrokerTargets(func() []BrokerTarget { return []BrokerTarget{{ProjectID: "p", BaseURL: "http://127.0.0.1:1"}} })
	c := h.checkBrokerAuditChannel(context.Background())
	if c.Status != "WARNING" || !strings.Contains(c.Message, "1 not reachable") {
		t.Fatalf("want WARNING with the not-reachable count, got %+v", c)
	}
}

func TestCheckBrokerAuditChannel_UnparseableIsWarning(t *testing.T) {
	url := brokerHealthServer(t, `not json`)
	h := NewDoctorHandlers(nil)
	h.SetBrokerTargets(func() []BrokerTarget { return []BrokerTarget{{ProjectID: "p", BaseURL: url}} })
	if c := h.checkBrokerAuditChannel(context.Background()); c.Status != "WARNING" {
		t.Fatalf("want WARNING, got %+v", c)
	}
}

// A /health without the audit fields (an older broker) cannot be read as
// healthy.
func TestCheckBrokerAuditChannel_MissingFieldsIsWarning(t *testing.T) {
	url := brokerHealthServer(t, `{"ok":true}`)
	h := NewDoctorHandlers(nil)
	h.SetBrokerTargets(func() []BrokerTarget { return []BrokerTarget{{ProjectID: "p", BaseURL: url}} })
	if c := h.checkBrokerAuditChannel(context.Background()); c.Status != "WARNING" {
		t.Fatalf("want WARNING, got %+v", c)
	}
}

// Two projects sharing one broker are probed once; a healthy broker is OK
// with the denominator.
func TestCheckBrokerAuditChannel_HealthyDedupesSharedBroker(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"ok":true,"audit_channel_auth_ok":true,"audit_pending":0}`))
	}))
	t.Cleanup(srv.Close)
	h := NewDoctorHandlers(nil)
	h.SetBrokerTargets(func() []BrokerTarget {
		return []BrokerTarget{{ProjectID: "a", BaseURL: srv.URL}, {ProjectID: "b", BaseURL: srv.URL}}
	})
	c := h.checkBrokerAuditChannel(context.Background())
	if c.Status != "OK" || hits != 1 || !strings.Contains(c.Message, "1 broker(s) examined") {
		t.Fatalf("want OK after one probe, got hits=%d %+v", hits, c)
	}
}

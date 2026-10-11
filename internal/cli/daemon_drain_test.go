package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// GitHub #290 (2026-10-08): the deploy gate sets the drain bit FIRST through
// `vornikctl daemon drain`, then reads the in-flight count, so no chat request
// can start uncounted between the read and the restart. `daemon undrain`
// reverses it when the deploy aborts. Exit codes: 0 done, 1 any error.

type drainSeen struct {
	method string
	body   string
}

func drainTestServer(t *testing.T, status int, body string) *drainSeen {
	t.Helper()
	seen := &drainSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/daemon/drain" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		seen.method, seen.body = r.Method, string(b)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
	return seen
}

func TestDaemonDrain_PostsTTLAndExitsZero(t *testing.T) {
	seen := drainTestServer(t, http.StatusOK,
		`{"draining":true,"shutting_down":false,"since":"2026-10-08T10:00:00Z","expires_at":"2026-10-08T10:02:00Z","chat_proxy":1,"total":1}`)
	daemonDrainTTL = 2 * time.Minute
	t.Cleanup(func() { daemonDrainTTL = defaultDaemonDrainTTL })
	var out bytes.Buffer
	daemonDrainCmd.SetOut(&out)
	if err := runDaemonDrain(daemonDrainCmd, nil); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if seen.method != http.MethodPost || !strings.Contains(seen.body, `"ttl_seconds":120`) {
		t.Fatalf("request = %s %s, want POST with ttl_seconds 120", seen.method, seen.body)
	}
	if !strings.Contains(out.String(), "draining: true") || !strings.Contains(out.String(), "in-flight chat-proxy requests: 1") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDaemonDrain_TTLOutOfRangeIsExitOne(t *testing.T) {
	drainTestServer(t, http.StatusOK, `{"draining":true}`)
	t.Cleanup(func() { daemonDrainTTL = defaultDaemonDrainTTL })
	for _, ttl := range []time.Duration{0, 500 * time.Millisecond, 61 * time.Minute, 1500 * time.Millisecond} {
		daemonDrainTTL = ttl
		if err := runDaemonDrain(daemonDrainCmd, nil); err == nil || ExitCodeOf(err) != 1 {
			t.Fatalf("ttl %v: exit = %d (err %v), want 1", ttl, ExitCodeOf(err), err)
		}
	}
}

func TestDaemonUndrain_SendsDeleteAndExitsZero(t *testing.T) {
	seen := drainTestServer(t, http.StatusOK, `{"draining":false}`)
	var out bytes.Buffer
	daemonUndrainCmd.SetOut(&out)
	if err := runDaemonUndrain(daemonUndrainCmd, nil); err != nil {
		t.Fatalf("undrain: %v", err)
	}
	if seen.method != http.MethodDelete {
		t.Fatalf("method = %s, want DELETE", seen.method)
	}
	if !strings.Contains(out.String(), "draining: false") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDaemonDrain_ErrorsAreExitOne(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"403":              {http.StatusForbidden, `{"error":{"code":"ADMIN_SCOPE_REQUIRED","message":"no"}}`},
		"409":              {http.StatusConflict, `{"error":{"code":"SHUTTING_DOWN","message":"no"}}`},
		"500":              {http.StatusInternalServerError, `{"error":{"code":"X","message":"boom"}}`},
		"unparseable":      {http.StatusOK, `{`},
		"missing draining": {http.StatusOK, `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			drainTestServer(t, tc.status, tc.body)
			if err := runDaemonDrain(daemonDrainCmd, nil); err == nil || ExitCodeOf(err) != 1 {
				t.Fatalf("drain exit = %d (err %v), want 1", ExitCodeOf(err), err)
			}
			if err := runDaemonUndrain(daemonUndrainCmd, nil); err == nil || ExitCodeOf(err) != 1 {
				t.Fatalf("undrain exit = %d (err %v), want 1", ExitCodeOf(err), err)
			}
		})
	}
}

// A drain body that reports draining:false is not a drain: exit 1.
func TestDaemonDrain_NotDrainingIsExitOne(t *testing.T) {
	drainTestServer(t, http.StatusOK, `{"draining":false}`)
	if err := runDaemonDrain(daemonDrainCmd, nil); err == nil || ExitCodeOf(err) != 1 {
		t.Fatalf("exit = %d (err %v), want 1", ExitCodeOf(err), err)
	}
}

func TestDaemonDrain_ConnectionRefusedIsExitOne(t *testing.T) {
	t.Setenv("VORNIK_API_URL", "http://127.0.0.1:1")
	if err := runDaemonDrain(daemonDrainCmd, nil); err == nil || ExitCodeOf(err) != 1 {
		t.Fatalf("drain exit = %d (err %v), want 1", ExitCodeOf(err), err)
	}
	if err := runDaemonUndrain(daemonUndrainCmd, nil); err == nil || ExitCodeOf(err) != 1 {
		t.Fatalf("undrain exit = %d (err %v), want 1", ExitCodeOf(err), err)
	}
}

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GitHub #586: `vornikctl daemon release` sends DELETE /api/v1/daemon/hold
// and prints "held: false"; any error, the daemon's refusals included,
// exits 1 (deploy idle gate design §13.4). Found at: review. Regression
// test at: unit.

func releaseTestServer(t *testing.T, status int, body string) *string {
	t.Helper()
	method := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/daemon/hold" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		*method = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
	return method
}

func TestDaemonRelease586_DeletesAndPrintsHeldFalse(t *testing.T) {
	method := releaseTestServer(t, http.StatusOK, `{"held":false,"overdue":false}`)
	var out bytes.Buffer
	daemonReleaseCmd.SetOut(&out)
	if err := runDaemonRelease(daemonReleaseCmd, nil); err != nil {
		t.Fatalf("release: %v", err)
	}
	if *method != http.MethodDelete || strings.TrimSpace(out.String()) != "held: false" {
		t.Fatalf("method %s, output %q", *method, out.String())
	}
}

func TestDaemonRelease586_RefusalsExitOne(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusInternalServerError, `{"error":{"code":"CONFIG_RELOAD_FAILED","message":"validation failed: x"}}`},
		{http.StatusInternalServerError, `{"error":{"code":"WORK_START_FAILED","message":"boom"}}`},
		{http.StatusConflict, `{"error":{"code":"RELEASE_FAILED","message":"x"}}`},
		{http.StatusForbidden, `{"error":{"code":"ADMIN_SCOPE_REQUIRED","message":"x"}}`},
		{http.StatusOK, `{"held":true}`},
		{http.StatusOK, `not json`},
	} {
		releaseTestServer(t, tc.status, tc.body)
		err := runDaemonRelease(daemonReleaseCmd, nil)
		if err == nil || ExitCodeOf(err) != 1 {
			t.Fatalf("%d %s: exit %d (%v), want 1", tc.status, tc.body, ExitCodeOf(err), err)
		}
		if tc.status == http.StatusInternalServerError && !strings.Contains(err.Error(), "_FAILED") {
			t.Fatalf("the daemon's reason is lost: %v", err)
		}
	}
}

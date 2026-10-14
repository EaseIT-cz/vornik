package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GitHub #306 (2026-10-08): `vornikctl daemon inflight` busy printed a bare
// "Error: " line from cobra. The fix silences cobra on the command and leaves
// main the one printer, so these run the real main() in a subprocess: busy is
// exit 2 with only the count line; a real error is exit 1 with its message
// printed exactly once.
func inflightDaemon(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
}

func TestMainDaemonInflight_BusyPrintsOnlyTheCountLine(t *testing.T) {
	inflightDaemon(t, http.StatusOK, `{"chat_proxy":1,"total":1,"draining":false}`)
	out, code := runMainSubprocess(t, "daemon", "inflight")
	if code != 2 {
		t.Fatalf("busy exit code = %d, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "in-flight chat-proxy requests: 1 (draining: false)\n") {
		t.Fatalf("busy output lacks the count line; output:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "error:") {
		t.Fatalf("busy output carries an error line; output:\n%s", out)
	}
}

func TestMainDaemonInflight_RealErrorPrintedOnce(t *testing.T) {
	inflightDaemon(t, http.StatusInternalServerError, `{"error":{"code":"X","message":"boom"}}`)
	out, code := runMainSubprocess(t, "daemon", "inflight")
	if code != 1 {
		t.Fatalf("error exit code = %d, want 1; output:\n%s", code, out)
	}
	if n := strings.Count(out, "boom"); n != 1 {
		t.Fatalf("error message printed %d times, want once; output:\n%s", n, out)
	}
	if !strings.Contains(out, "error: ") {
		t.Fatalf("main's error line missing; output:\n%s", out)
	}
}

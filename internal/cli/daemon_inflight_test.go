package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GitHub #97 (2026-10-07 deploy cut an in-flight chat stream): the deploy
// idle gate reads `vornikctl daemon inflight`. Its exit code is the contract:
// 0 idle, 2 busy, 1 any error, so a failed read is never taken for idle (the
// 2026-10-01 lesson, where a 401 read as idle).

func inflightServer(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/daemon/inflight" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
}

func TestDaemonInflight_PrintsCountAndExitsNonZeroWhenBusy(t *testing.T) {
	inflightServer(t, http.StatusOK, `{"chat_proxy":1,"total":1,"draining":false}`)
	var out bytes.Buffer
	daemonInflightCmd.SetOut(&out)
	err := runDaemonInflight(daemonInflightCmd, nil)
	if got := ExitCodeOf(err); got != 2 {
		t.Fatalf("busy exit code = %d (err %v), want 2", got, err)
	}
	if !strings.Contains(out.String(), "in-flight chat-proxy requests: 1") {
		t.Fatalf("output: %q", out.String())
	}

	inflightServer(t, http.StatusOK, `{"chat_proxy":0,"total":0,"draining":false}`)
	if err := runDaemonInflight(daemonInflightCmd, nil); ExitCodeOf(err) != 0 {
		t.Fatalf("idle exit code = %d (err %v), want 0", ExitCodeOf(err), err)
	}

	// GitHub #306 (2026-10-08, reference host after deploying 3ad00f711): busy
	// also printed a bare "Error: " line from cobra, so a busy daemon read like
	// a failure. Through the real command tree, busy is exit 2, the count line
	// on stdout, and nothing on stderr; main prints only a non-empty message.
	inflightServer(t, http.StatusOK, `{"chat_proxy":1,"total":1,"draining":false}`)
	stdout, stderr, err := executeInflight(t)
	if got := ExitCodeOf(err); got != 2 {
		t.Fatalf("busy exit code via Execute = %d (err %v), want 2", got, err)
	}
	if err.Error() != "" {
		t.Fatalf("busy error message = %q, want empty (main would print it)", err.Error())
	}
	if !strings.Contains(stdout, "in-flight chat-proxy requests: 1") {
		t.Fatalf("busy stdout: %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("busy stderr = %q, want nothing (no bare \"Error: \" line)", stderr)
	}
}

// executeInflight runs `daemon inflight` through rootCmd, as vornikctl's main
// does, capturing cobra's stdout and stderr.
func executeInflight(t *testing.T) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errb)
	// An earlier SetOut on the subcommand would win over root's; clear it so
	// the command writes to the buffers set on root.
	daemonInflightCmd.SetOut(nil)
	rootCmd.SetArgs([]string{"daemon", "inflight"})
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil) })
	err := Execute()
	return out.String(), errb.String(), err
}

// GitHub #306: a real error still carries its message and exits 1, and cobra
// does not print an "Error: " copy of it. That main then prints it exactly
// once is pinned through the real main() in cmd/vornikctl
// (TestMainDaemonInflight_RealErrorPrintedOnce).
func TestDaemonInflight_RealErrorKeepsMessageWithoutCobraCopy(t *testing.T) {
	inflightServer(t, http.StatusInternalServerError, `{"error":{"code":"X","message":"boom"}}`)
	_, stderr, err := executeInflight(t)
	if ExitCodeOf(err) != 1 {
		t.Fatalf("exit code = %d (err %v), want 1", ExitCodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error message %q lost the daemon's message", err.Error())
	}
	if strings.Contains(stderr, "Error:") {
		t.Fatalf("cobra printed the error too (main prints it): %q", stderr)
	}
}

func TestDaemonInflight_ErrorIsExitOne(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"401":           {http.StatusUnauthorized, `{"error":{"code":"UNAUTHORIZED","message":"no"}}`},
		"500":           {http.StatusInternalServerError, `{"error":{"code":"X","message":"boom"}}`},
		"unparseable":   {http.StatusOK, `{`},
		"missing total": {http.StatusOK, `{"draining":false}`},
	} {
		t.Run(name, func(t *testing.T) {
			inflightServer(t, tc.status, tc.body)
			err := runDaemonInflight(daemonInflightCmd, nil)
			if err == nil || ExitCodeOf(err) != 1 {
				t.Fatalf("exit code = %d (err %v), want 1", ExitCodeOf(err), err)
			}
		})
	}
}

func TestDaemonInflight_ConnectionRefusedIsExitOne(t *testing.T) {
	t.Setenv("VORNIK_API_URL", "http://127.0.0.1:1")
	if err := runDaemonInflight(daemonInflightCmd, nil); err == nil || ExitCodeOf(err) != 1 {
		t.Fatalf("exit code = %d (err %v), want 1", ExitCodeOf(err), err)
	}
}

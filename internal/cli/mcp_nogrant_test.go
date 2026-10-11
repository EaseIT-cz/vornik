package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// GitHub #91 (2026-10-08), auth design §14: an oauth server with no grant is
// not dialled. `vornikctl mcp servers` shows its own state (not
// "unreachable"), and `oauth-status` says why and how to fix it, with the
// same remedy string as the daemon log and the doctor.

func captureNoGrantStdout(t *testing.T, run func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	runErr := run()
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = old
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	return buf.String()
}

func stubDaemon(t *testing.T, body string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test")
}

func TestRunMCPServers_NotDialledHasItsOwnStatus(t *testing.T) {
	body := `{"servers":[{"name":"atlassian","transport":"streamable-http","url":"https://mcp.atlassian.com/v1/mcp",` +
		`"reachable":false,"not_dialled":true,"error":"not dialled: not connected, no grant: run vornikctl mcp connect atlassian",` +
		`"last_checked_at":"2026-10-08T10:00:00Z","tools":null}]}`
	stubDaemon(t, body)
	mcpJSON = false
	got := captureNoGrantStdout(t, func() error { return runMCPServers(mcpServersCmd, nil) })
	if !strings.Contains(got, "not connected, no grant") || strings.Contains(got, "unreachable") {
		t.Fatalf("status:\n%s", got)
	}
	// --json keeps the daemon's shape, not_dialled included.
	mcpJSON = true
	t.Cleanup(func() { mcpJSON = false })
	raw := captureNoGrantStdout(t, func() error { return runMCPServers(mcpServersCmd, nil) })
	if !strings.Contains(raw, `"not_dialled": true`) {
		t.Fatalf("--json lost not_dialled:\n%s", raw)
	}
}

func TestRunMCPOAuthStatus_NoGrantNamesTheRemedy(t *testing.T) {
	stubDaemon(t, `{"connected":false}`)
	mcpJSON = false
	for project, want := range map[string]string{
		"":          "Run: vornikctl mcp connect atlassian\n",
		"assistant": "Run: vornikctl mcp connect atlassian -p assistant\n",
	} {
		mcpProject = project
		got := captureNoGrantStdout(t, func() error { return runMCPOAuthStatus(nil, []string{"atlassian"}) })
		if !strings.Contains(got, "no grant is stored, so the daemon does not dial it") || !strings.Contains(got, want) {
			t.Errorf("project %q:\n%s", project, got)
		}
	}
	// Review 030f B3: an agent project signs in on the phone.
	mcpProject = "hermes--venues"
	got := captureNoGrantStdout(t, func() error { return runMCPOAuthStatus(nil, []string{"calendar"}) })
	if !strings.Contains(got, "To connect it: sign in on the approver device") || strings.Contains(got, "Run: vornikctl") {
		t.Errorf("agent project:\n%s", got)
	}
	mcpProject = ""
}

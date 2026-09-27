package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/leaderelection"
)

// `vornikctl leader-lock release` (issue #60; horizontal scaling LLD,
// implementation contract 2026-09-25). The CLI decides nothing: it sends the
// request, prints the per-row answer and turns it into an exit code a script
// can act on. vornikctl used to exit 1 for every error, so no command could
// return anything else; ExitCodeOf is what main now honours.

func llStub(t *testing.T, status int, body any, gotReq *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/leader-locks/release" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if gotReq != nil {
			_ = json.NewDecoder(r.Body).Decode(gotReq)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func llRun(t *testing.T, srvURL string, args []string, all bool, reason string) (string, error) {
	t.Helper()
	if srvURL != "" {
		t.Setenv("VORNIK_API_URL", srvURL)
		t.Setenv("VORNIK_API_KEY", "sk-admin")
	}
	var out bytes.Buffer
	err := runLeaderLockRelease(args, all, reason, false, &out)
	return out.String(), err
}

func TestLeaderLockReleaseCLI_UsageIsExit2WithoutARequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a usage error must not reach the daemon")
	}))
	defer srv.Close()
	for name, c := range map[string]struct {
		args   []string
		all    bool
		reason string
	}{
		"neither":                 {nil, false, ""},
		"both":                    {[]string{"w"}, true, "r"},
		"all-orphaned, no reason": {nil, true, ""},
	} {
		if _, err := llRun(t, srv.URL, c.args, c.all, c.reason); ExitCodeOf(err) != leaderelection.ExitUsage {
			t.Errorf("%s: exit %d, want %d (%v)", name, ExitCodeOf(err), leaderelection.ExitUsage, err)
		}
	}
}

func TestLeaderLockReleaseCLI_ExitCodeFollowsTheOutcomes(t *testing.T) {
	var sent map[string]any
	srv := llStub(t, http.StatusOK, map[string]any{"results": []map[string]any{
		{"worker_id": "a", "outcome": leaderelection.OutcomeReleased, "message": "released from host-a (epoch 7)"},
		{"worker_id": "typo", "outcome": leaderelection.OutcomeUnknown, "message": "no leader lock for this worker id"},
		{"worker_id": "b", "outcome": leaderelection.OutcomeRefusedActive, "message": "restart the holder"},
	}}, &sent)
	defer srv.Close()
	out, err := llRun(t, srv.URL, []string{"a", "typo", "b"}, false, "issue 60")
	if got := ExitCodeOf(err); got != leaderelection.ExitActive {
		t.Fatalf("a typo must not hide a live holder: exit %d, want %d", got, leaderelection.ExitActive)
	}
	for _, want := range []string{"released", "a", "refused-active", "unknown", "1 released, 2 refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if sent["reason"] != "issue 60" || len(sent["worker_ids"].([]any)) != 3 {
		t.Errorf("request body: %v", sent)
	}
}

func TestLeaderLockReleaseCLI_ZeroOrphansSaysSo(t *testing.T) {
	srv := llStub(t, http.StatusOK, map[string]any{"results": []any{}}, nil)
	defer srv.Close()
	out, err := llRun(t, srv.URL, nil, true, "sweep")
	if ExitCodeOf(err) != leaderelection.ExitReleased || !strings.Contains(out, "0 orphaned rows found") {
		t.Fatalf("exit %d out %q", ExitCodeOf(err), out)
	}
}

func TestLeaderLockReleaseCLI_ServerAnswers(t *testing.T) {
	for status, want := range map[int]int{
		http.StatusBadRequest:          leaderelection.ExitUsage,
		http.StatusInternalServerError: leaderelection.ExitError,
		http.StatusForbidden:           leaderelection.ExitError,
	} {
		srv := llStub(t, status, map[string]any{"error": map[string]any{"code": "X", "message": "nope"}}, nil)
		_, err := llRun(t, srv.URL, []string{"a"}, false, "")
		srv.Close()
		if got := ExitCodeOf(err); got != want {
			t.Errorf("HTTP %d: exit %d, want %d", status, got, want)
		}
	}
}

// No --force: an unknown flag is a usage error, exit 2, never silently ignored.
func TestLeaderLockReleaseCLI_ForceIsAnUnknownFlag(t *testing.T) {
	err := leaderLockReleaseCmd.ParseFlags([]string{"--force"})
	if err == nil {
		t.Fatal("--force must not parse")
	}
	if got := ExitCodeOf(leaderLockReleaseCmd.FlagErrorFunc()(leaderLockReleaseCmd, err)); got != leaderelection.ExitUsage {
		t.Fatalf("exit %d, want %d", got, leaderelection.ExitUsage)
	}
}

func TestExitCodeOf(t *testing.T) {
	if ExitCodeOf(nil) != 0 || ExitCodeOf(errors.New("x")) != 1 || ExitCodeOf(&featureExitError{code: 1}) != 1 {
		t.Fatal("nil is 0, a plain error 1, featureExitError its code")
	}
	if ExitCodeOf(&exitCodeError{code: 4}) != 4 {
		t.Fatal("an exitCodeError carries its code")
	}
}

func TestLeaderLockReleaseCLI_AuditFailureExits1(t *testing.T) {
	srv := llStub(t, http.StatusOK, map[string]any{"results": []map[string]any{
		{"worker_id": "a", "outcome": leaderelection.OutcomeReleased, "message": "released", "audit_error": "audit sink down"},
	}}, nil)
	defer srv.Close()
	out, err := llRun(t, srv.URL, []string{"a"}, false, "")
	if ExitCodeOf(err) != leaderelection.ExitError || !strings.Contains(out, "AUDIT WRITE FAILED") {
		t.Fatalf("exit %d out %q", ExitCodeOf(err), out)
	}
}

// main prints the error once; cobra must not print it again (a bare "Error: "
// line on every silent exit, and each flag error twice, on the first deploy).
func TestLeaderLockReleaseCLI_ErrorsPrintOnce(t *testing.T) {
	if !leaderLockReleaseCmd.SilenceErrors {
		t.Fatal("leader-lock release must leave error printing to main")
	}
}

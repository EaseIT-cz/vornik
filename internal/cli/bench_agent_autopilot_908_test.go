package cli

// GitHub #908: the bench autopilot's generate call used the CLI's 30 s
// default and gave up before the daemon's 60 s handler cut the producer; 12 of
// 12 calls failed, and each was journaled as no_candidate, so the arm read as
// "nothing to fix".
//
// Found at: production (bench arm C exploratory run, 2026-11-24).
// Regression test at: integration (the command over HTTP against an httptest
// daemon) and unit (the clients' deadlines).
// Lower because: a production reproduction needs a full warm-up pass and a
// remote model; the defects are the client's deadline and the journal's
// classification, which these tests read directly.

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

func TestClientWithMinTimeout_908(t *testing.T) {
	for _, tc := range []struct {
		name      string
		have, min time.Duration
		want      time.Duration
	}{
		{"raises a shorter deadline", 30 * time.Second, 8 * time.Minute, 8 * time.Minute},
		{"never lowers a longer one", 20 * time.Minute, 8 * time.Minute, 20 * time.Minute},
		{"keeps a disabled deadline disabled", 0, 8 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClientWithTimeout("http://x", "k", tc.have).WithMinTimeout(tc.min)
			if got := c.httpClient.Timeout; got != tc.want {
				t.Fatalf("timeout = %s, want %s", got, tc.want)
			}
		})
	}
}

// The operator command cannot read the daemon's config: it outlives the
// default budget, and a longer VORNIK_API_TIMEOUT still wins.
func TestBlackboxGenerateClient_OutlivesTheDefaultBudget_908(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", config.DefaultHealingGenerateBudget + generateClientSlack},
		{"20m", 20 * time.Minute},
		{"10s", config.DefaultHealingGenerateBudget + generateClientSlack},
		{"0", 0},
	} {
		t.Setenv("VORNIK_API_TIMEOUT", tc.env)
		if got := generateCandidateClient().httpClient.Timeout; got != tc.want {
			t.Fatalf("VORNIK_API_TIMEOUT=%q: timeout = %s, want %s", tc.env, got, tc.want)
		}
	}
}

// The autopilot's generate client outlives the budget of the daemon whose
// config it was given.
func TestAutopilotGenerateClient_OutlivesTheDaemonsBudget_908(t *testing.T) {
	t.Setenv("VORNIK_API_TIMEOUT", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("config_assistant:\n  request_timeout: 10m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := autopilotGenerateClient(path)
	if err != nil {
		t.Fatal(err)
	}
	budget := config.AssistantConfig{RequestTimeout: "10m"}.HealingGenerateBudget()
	if got := c.httpClient.Timeout; got <= budget {
		t.Fatalf("the generate client gives up at %s, before the daemon's budget %s", got, budget)
	}
}

// A failed generate call is journaled as failed, not no_candidate, and the
// run's summary says the arm produced nothing.
func TestAutopilot_AFailedGenerateIsReportedAsFailed_908(t *testing.T) {
	runDir, f := setupAutopilot809(t)
	f.genStatus = http.StatusBadGateway
	var out bytes.Buffer
	benchAgentAutopilotCmd.SetOut(&out)
	if err := runBenchAgentAutopilot(benchAgentAutopilotCmd, nil); err != nil {
		t.Fatalf("one failed generate call must not fail the run: %v", err)
	}
	if got := autopilotSteps809(t, runDir); got != "intent:,trigger:,generate:failed,dismiss:dismissed" {
		t.Fatalf("steps = %s; calls %v", got, f.calls)
	}
	for _, want := range []string{"1 of 1 failed", "ran as a baseline"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the run's output does not say %q:\n%s", want, out.String())
		}
	}
}

// Only the producer's own "made nothing" answers are no_candidate; a call
// that never reached the producer is failed (review-20261124-a05c F1).
func TestGenerateVerdict_908(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&APIError{StatusCode: http.StatusUnprocessableEntity}, "no_candidate"},
		{&APIError{StatusCode: http.StatusConflict}, "no_candidate"},
		{&APIError{StatusCode: http.StatusBadRequest}, "failed"},
		{&APIError{StatusCode: http.StatusUnauthorized}, "failed"},
		{&APIError{StatusCode: http.StatusNotFound}, "failed"},
		{&APIError{StatusCode: http.StatusTooManyRequests}, "failed"},
		{&APIError{StatusCode: http.StatusBadGateway}, "failed"},
		{errors.New("request failed: context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), "failed"},
	} {
		if got := generateVerdict(tc.err); got != tc.want {
			t.Errorf("generateVerdict(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}

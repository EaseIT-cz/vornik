package agentbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
)

// mcpReply wraps a payload the way the companion MCP surface does.
func mcpReply(t *testing.T, payload any) string {
	t.Helper()
	inner, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	outer, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"content": []map[string]string{{"type": "text", "text": string(inner)}},
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(outer)
}

// mcpServer replies to `delegate` once, then to `status` from a script.
//
// It refuses an argument the tool does not declare, with the daemon's own
// check (api.CheckCompanionToolArgs). Until 2026-10-14 it read only the tool
// name, so delegate's "project" (never declared; silently dropped before
// GitHub #305, refused after) passed here while every submit against a
// 2026.10.6 daemon would fail.
func mcpServer(t *testing.T, statuses []map[string]any) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		seen = append(seen, req.Params.Name)

		w.Header().Set("Content-Type", "application/json")
		if err := api.CheckCompanionToolArgs(req.Params.Name, req.Params.Arguments); err != nil {
			refused, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
				"isError": true, "content": []map[string]string{{"type": "text", "text": err.Error()}}}})
			_, _ = w.Write(refused)
			return
		}
		switch req.Params.Name {
		case "delegate":
			_, _ = w.Write([]byte(mcpReply(t, map[string]any{
				"task_id": "task_1", "status": "QUEUED",
			})))
		case "status":
			s := statuses[len(statuses)-1]
			if i < len(statuses) {
				s = statuses[i]
			}
			i++
			_, _ = w.Write([]byte(mcpReply(t, s)))
		default:
			t.Errorf("unexpected tool %q", req.Params.Name)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// assertDeclaredArgs fails the test when a fake records a call whose
// arguments the daemon would refuse (api.CheckCompanionToolArgs; GitHub #305
// follow-up, 2026-10-14).
func assertDeclaredArgs(t *testing.T, tool string, args map[string]any) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal %s arguments: %v", tool, err)
	}
	if err := api.CheckCompanionToolArgs(tool, raw); err != nil {
		t.Errorf("the daemon would refuse this call: %v", err)
	}
}

func testRunner(t *testing.T, srv *httptest.Server) *DaemonTaskRunner {
	t.Helper()
	return NewDaemonTaskRunner(DaemonConfig{
		BaseURL:      srv.URL,
		Token:        "tok",
		Project:      "bench",
		PollInterval: time.Millisecond,
		Timeout:      2 * time.Second,
		HTTPClient:   srv.Client(),
	})
}

func TestDaemonTaskRunner_SubmitsThenPollsToCompletion(t *testing.T) {
	srv, seen := mcpServer(t, []map[string]any{
		{"task_id": "task_1", "status": "RUNNING"},
		{"task_id": "task_1", "status": "RUNNING"},
		{"task_id": "task_1", "status": "COMPLETED", "execution_ids": []string{"e1", "e2"}},
	})

	out, err := testRunner(t, srv).Run(context.Background(), TaskSpec{ID: "t1", Workflow: "w", Prompt: "p"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Succeeded {
		t.Error("a COMPLETED task was reported as failed")
	}
	// The companion status payload does NOT report execution ids. An earlier
	// version parsed an invented execution_ids field, so every run assembled
	// nothing and printed zeroes while claiming success. The runner resolves
	// executions from the ledger instead; this pins that the adapter does not
	// pretend to know them.
	if len(out.Executions) != 0 {
		t.Errorf("executions = %v, want none — the daemon does not report them, and "+
			"inventing the field is how a run silently measures nothing", out.Executions)
	}
	if (*seen)[0] != "delegate" {
		t.Errorf("first call was %q, want delegate", (*seen)[0])
	}
	if len(*seen) != 4 {
		t.Errorf("calls = %v; want one submit and three polls", *seen)
	}
}

func TestDaemonTaskRunner_FailedTaskCarriesItsErrorAndExecutions(t *testing.T) {
	srv, _ := mcpServer(t, []map[string]any{{
		"task_id": "task_1", "status": "FAILED",
		"last_error": "acceptance criteria not met",
	}})

	out, err := testRunner(t, srv).Run(context.Background(), TaskSpec{ID: "t1"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Succeeded {
		t.Error("a FAILED task was reported as succeeded")
	}
	if out.ErrorText != "acceptance criteria not met" {
		t.Errorf("error text = %q", out.ErrorText)
	}
	// The trace still exists and is still worth assembling — via the ledger,
	// which is what the runner asks.
	if out.TaskID == "" {
		t.Error("the task id was dropped, so the ledger cannot be asked for its executions")
	}
	if got := ClassifyFailure(out.Succeeded, out.ErrorText); got != FailureTask {
		t.Errorf("classified as %q, want task", got)
	}
}

// A terminal failure with no recorded reason must not read as the agent failing.
func TestDaemonTaskRunner_FailureWithNoReasonClassifiesAsHarness(t *testing.T) {
	srv, _ := mcpServer(t, []map[string]any{{"task_id": "task_1", "status": "FAILED"}})

	out, err := testRunner(t, srv).Run(context.Background(), TaskSpec{ID: "t1"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := ClassifyFailure(out.Succeeded, out.ErrorText); got != FailureHarness {
		t.Errorf("classified as %q, want harness — an unexplained failure is not "+
			"evidence the agent failed", got)
	}
}

// A timeout is an OUTCOME, not an error: the task ran, and the arm continues.
func TestDaemonTaskRunner_TimeoutIsAnOutcomeNotAnAbort(t *testing.T) {
	srv, _ := mcpServer(t, []map[string]any{{"task_id": "task_1", "status": "RUNNING"}})

	r := NewDaemonTaskRunner(DaemonConfig{
		BaseURL: srv.URL, Token: "tok",
		PollInterval: time.Millisecond, Timeout: 5 * time.Millisecond,
		HTTPClient: srv.Client(),
	})

	out, err := r.Run(context.Background(), TaskSpec{ID: "t1"})
	if err != nil {
		t.Fatalf("a timeout aborted the arm instead of being recorded: %v", err)
	}
	if out.Succeeded || !strings.Contains(out.ErrorText, "timeout") {
		t.Errorf("outcome = %+v, want a recorded timeout", out)
	}
}

func TestDaemonTaskRunner_RefusesWithoutCredentials(t *testing.T) {
	r := NewDaemonTaskRunner(DaemonConfig{})
	if _, err := r.Run(context.Background(), TaskSpec{ID: "t1"}); err == nil {
		t.Fatal("submitted with no base URL and no token")
	}
}

func TestDaemonTaskRunner_SurfacesADaemonError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	r := NewDaemonTaskRunner(DaemonConfig{
		BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client(),
	})
	_, err := r.Run(context.Background(), TaskSpec{ID: "t1"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("want the HTTP status surfaced, got: %v", err)
	}
}

func TestDaemonTaskRunner_HonoursContextCancellation(t *testing.T) {
	srv, _ := mcpServer(t, []map[string]any{{"task_id": "task_1", "status": "RUNNING"}})

	ctx, cancel := context.WithCancel(context.Background())
	r := NewDaemonTaskRunner(DaemonConfig{
		BaseURL: srv.URL, Token: "tok",
		PollInterval: 50 * time.Millisecond, Timeout: time.Minute,
		HTTPClient: srv.Client(),
	})

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := r.Run(ctx, TaskSpec{ID: "t1"}); err == nil {
		t.Fatal("a cancelled run kept polling")
	}
}

// The check that was missing on 2026-08-12: naming a database proves nothing
// about where the daemon writes.
func TestDaemonTaskRunner_ReportsItsWriteTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(mcpReply(t, map[string]any{"database": "other_database"})))
	}))
	defer srv.Close()

	r := NewDaemonTaskRunner(DaemonConfig{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client()})
	got, err := r.WriteTargetDatabase(context.Background())
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if got != "other_database" {
		t.Errorf("write target = %q, want the database the daemon reported", got)
	}
}

// A daemon that cannot answer is refused, not waved through: "unverified" is not
// "safe", and treating it as safe is what let a benchmark write production.
func TestDaemonTaskRunner_WriteTargetFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewDaemonTaskRunner(DaemonConfig{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client()})
	if _, err := r.WriteTargetDatabase(context.Background()); err == nil {
		t.Fatal("a daemon that could not report its write target was accepted")
	}
}

// GitHub #378, review-20261014-281d suggestion: the key-project check fails
// closed when whoami cannot be asked (no cached reply, daemon errors) and
// when no benchmark project is named.
func TestDaemonTaskRunner_KeyProjectFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewDaemonTaskRunner(DaemonConfig{BaseURL: srv.URL, Token: "tok", Project: "bench", HTTPClient: srv.Client()})
	err := r.VerifyKeyProject(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"bench"`) {
		t.Fatalf("err = %v, want a refusal naming the project", err)
	}
	r = NewDaemonTaskRunner(DaemonConfig{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client()})
	if err := r.VerifyKeyProject(context.Background()); err == nil {
		t.Fatal("no --project was accepted")
	}
}

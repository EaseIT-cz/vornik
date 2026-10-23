package dispatcher

// GitHub #395 — transcript secret scanning had gaps at write time.
// Found at: review (the #344 security follow-up, Art 17 redact design
// §15.11.4a). Observed at: integration — chat_audit_log rows persisted
// holding a credential. These tests sit at that level: the REAL turn writer
// (chatAuditTurn / Process) writes through the decorator the container
// installs into a REAL SQLite chat_audit_log, and the stored row is read
// back. Design: 2026-09-04-chat-audit-retention-and-redaction-design.md §9.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// Synthetic credentials of the right shape; never real.
const (
	gh395  = "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
	aws395 = "AKIAQ3EGUZJ7RLW4XN2P"
)

func detector395(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:  secrets.EffectivePatterns(nil, nil),
		Allowlist: secrets.DefaultAllowlist(),
	})
	require.NoError(t, err)
	require.Len(t, gh395, 40, "fixture: ghp_ + 36")
	require.NotEmpty(t, d.Scan([]byte("x "+gh395+" y")), "fixture must match github_pat")
	require.NotEmpty(t, d.Scan([]byte("x "+aws395+" y")), "fixture must match aws_access_key")
	return d
}

// chatAuditDB395 is the real store behind the decorator the container wires.
func chatAuditDB395(t *testing.T) (persistence.ChatAuditRepository, *sql.DB) {
	t.Helper()
	db := sqlitetest.Memory(t)
	inner := sqlite.NewChatAuditRepository(db.DB)
	return auditredact.NewChatAudit(inner, detector395(t), nil, nil), db.DB
}

// assertNoFragment fails when any 10-byte window of secret is in stored.
// A fragment that short matches no pattern any more, which is exactly why a
// cut-before-scan leak is invisible to every later scan.
func assertNoFragment(t *testing.T, field, stored, secret string) {
	t.Helper()
	for i := 0; i+10 <= len(secret); i++ {
		if strings.Contains(stored, secret[i:i+10]) {
			t.Fatalf("%s stores a fragment of the secret (%q):\n%s", field, secret[i:i+10], stored)
		}
	}
}

// storedRow395 reads the stored bytes straight from the table: the
// repository's GetByID returns only the identity columns.
func storedRow395(t *testing.T, db *sql.DB, id string) *persistence.ChatAuditEntry {
	t.Helper()
	e := &persistence.ChatAuditEntry{ID: id}
	require.NoError(t, db.QueryRow(`SELECT user_message, response, tool_calls_json,
		COALESCE(hallucination_signals_json, '') FROM chat_audit_log WHERE id = ?`, id).
		Scan(&e.UserMessage, &e.Response, &e.ToolCallsJSON, &e.HallucinationSignalsJSON))
	return e
}

// A credential straddling the 500-byte user_message cut was cut first and
// scanned second: the stored head no longer matched github_pat.
func TestChatAudit395_UserMessageSecretStraddlingTheCutIsNotStored(t *testing.T) {
	repo, inner := chatAuditDB395(t)
	a := &Agent{chatAuditRepo: repo, logger: zerolog.Nop()}
	turn := newChatAuditTurn(a)
	user := strings.Repeat("a", 469) + " " + gh395 + " and more text after it"
	turn.captureRequest("system", user, "dispatcher")
	turn.finish(context.Background(), Request{Project: "p1"}, Result{Text: "ok"})

	row := storedRow395(t, inner, turn.id)
	assertNoFragment(t, "user_message", row.UserMessage, gh395)
	require.LessOrEqual(t, len(row.UserMessage), 500, "the stored limit still holds")
	require.True(t, utf8.ValidString(row.UserMessage))
}

// The same for a tool argument and a tool result, cut at 1024.
func TestChatAudit395_ToolFieldsScannedBeforeTheCut(t *testing.T) {
	repo, inner := chatAuditDB395(t)
	a := &Agent{chatAuditRepo: repo, logger: zerolog.Nop()}
	turn := newChatAuditTurn(a)
	args := `{"body":"` + strings.Repeat("b", 989) + " " + gh395 + `"}`
	result := strings.Repeat("r", 999) + " " + aws395 + " tail"
	turn.recordToolCall("send_mail", args, result, "")
	turn.finish(context.Background(), Request{Project: "p1"}, Result{Text: "ok"})

	row := storedRow395(t, inner, turn.id)
	assertNoFragment(t, "tool_calls_json", row.ToolCallsJSON, gh395)
	assertNoFragment(t, "tool_calls_json", row.ToolCallsJSON, aws395)
	var calls []chatAuditToolCall
	require.NoError(t, json.Unmarshal([]byte(row.ToolCallsJSON), &calls), "the column stays the writer's JSON shape")
	require.Len(t, calls, 1)
	require.LessOrEqual(t, len(calls[0].Args), 1024)
	require.LessOrEqual(t, len(calls[0].Result), 1024)
}

// Review cc74 F1: a credential longer than the window margin, starting before
// the stored limit and running past the writer's window, must leave no head:
// CutBeforeScan drops the run that straddles the window edge.
func TestChatAudit395_CredentialLongerThanTheWindowLeavesNoHead(t *testing.T) {
	repo, inner := chatAuditDB395(t)
	a := &Agent{chatAuditRepo: repo, logger: zerolog.Nop()}
	turn := newChatAuditTurn(a)
	long := "eyJ" + strings.Repeat("Qk9", 1200) // one 3603-byte secret-alphabet run
	turn.recordToolCall("fetch", "{}", strings.Repeat("r", 949)+" "+long+" tail", "")
	turn.finish(context.Background(), Request{Project: "p1"}, Result{Text: "ok"})
	row := storedRow395(t, inner, turn.id)
	assertNoFragment(t, "tool_calls_json", row.ToolCallsJSON, long[:40])
}

// hallucination_signals_json was never scanned: a key the model repeated in a
// claim was stored verbatim, and one cut at 256 bytes as a fragment.
func TestChatAudit395_HallucinationSignalsAreScanned(t *testing.T) {
	repo, inner := chatAuditDB395(t)
	a := &Agent{chatAuditRepo: repo, logger: zerolog.Nop()}
	turn := newChatAuditTurn(a)
	reply := "Fetched https://api.example.test/v1?key=" + aws395 + " and the token " + gh395 + " works."
	turn.captureRequest("system", "check it", "dispatcher")
	turn.recordHallucinationSignals([]hallucination.Signal{
		// Whole key in the claim and the sentence.
		hallucination.NewSignal("url_not_fetched", hallucination.SeverityWarn, "url",
			"https://api.example.test/v1?key="+aws395, reply, "tool_audit (0)", "URL was not fetched"),
		// The claim cut at 256 bytes, mid-token: only a fragment survives the cut.
		hallucination.NewSignal("url_not_fetched", hallucination.SeverityWarn, "url",
			"https://x.test/"+strings.Repeat("p", 225)+gh395, "", "tool_audit (0)", "URL was not fetched"),
	})
	turn.finish(context.Background(), Request{Project: "p1"}, Result{Text: reply})

	row := storedRow395(t, inner, turn.id)
	require.NotEmpty(t, row.HallucinationSignalsJSON)
	assertNoFragment(t, "hallucination_signals_json", row.HallucinationSignalsJSON, aws395)
	assertNoFragment(t, "hallucination_signals_json", row.HallucinationSignalsJSON, gh395)
	var signals []map[string]any
	require.NoError(t, json.Unmarshal([]byte(row.HallucinationSignalsJSON), &signals), "the column stays JSON")
	require.Len(t, signals, 2)
}

// A sentence starts after the previous '.', and a JWT has dots: the quoted
// sentence can begin inside a token, with no pattern able to match its tail.
func TestChatAudit395_SignalSentenceStartingInsideATokenIsRedacted(t *testing.T) {
	repo, inner := chatAuditDB395(t)
	a := &Agent{chatAuditRepo: repo, logger: zerolog.Nop()}
	turn := newChatAuditTurn(a)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJvcGVyYXRvciIsImV4cCI6MTk5OTk5OTk5OX0.k3Jm9QvT7xW2pL5nR8sZ1cY4uB6dH0gA"
	tail := jwt[strings.LastIndex(jwt, ".")+1:]
	reply := "Use this token " + jwt + " to call the API"
	turn.captureRequest("system", "token?", "dispatcher")
	turn.recordHallucinationSignals([]hallucination.Signal{
		hallucination.NewSignal("tool_claim", hallucination.SeverityWarn, "url", "api", tail+" to call the API", "", "claimed a call"),
	})
	turn.finish(context.Background(), Request{Project: "p1"}, Result{Text: reply})

	row := storedRow395(t, inner, turn.id)
	assertNoFragment(t, "hallucination_signals_json", row.HallucinationSignalsJSON, tail)
}

// The chat-audit tool result was recorded from the PRE-guard content, so what
// the output guard redacted at HIGH reached chat_audit_log anyway.
func TestChatAudit395_ToolResultIsRecordedAfterTheOutputGuard(t *testing.T) {
	// A HIGH secret-class finding (data_uri_payload) the guard redacts in
	// place whatever the provenance — list_projects is first-party, so an
	// injection phrase would be skipped. The secrets detector has no rule for
	// it, so only the guard can keep it out of the row, which isolates the
	// ordering under test.
	const dataURI = "data:text/html;base64,"
	repo, inner := chatAuditDB395(t)
	turnID := runOneToolTurn395(t, repo, dataURI+"PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==", WithOutputGuard(true))
	row := storedRow395(t, inner, turnID())
	require.NotContains(t, row.ToolCallsJSON, dataURI, "what the guard redacted must not reach the audit row")
	require.Contains(t, row.ToolCallsJSON, "list_projects")
}

// The loop contract when no guard is wired: the recorded result is the tool's
// own content.
func TestChatAudit395_NoGuardRecordsTheToolContent(t *testing.T) {
	repo, inner := chatAuditDB395(t)
	turnID := runOneToolTurn395(t, repo, "Plain Display Name")
	row := storedRow395(t, inner, turnID())
	var calls []chatAuditToolCall
	require.NoError(t, json.Unmarshal([]byte(row.ToolCallsJSON), &calls))
	require.Len(t, calls, 1)
	require.Contains(t, calls[0].Result, "Plain Display Name")
}

// runOneToolTurn395 drives the real Process loop: the model calls
// list_projects once (whose result carries displayName), then answers.
func runOneToolTurn395(t *testing.T, repo persistence.ChatAuditRepository, displayName string, opts ...AgentOption) func() string {
	t.Helper()
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt64(&n, 1) == 1 {
			_, _ = w.Write([]byte(`{"id":"r1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"tc1","type":"function","function":{"name":"list_projects","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"r2","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)

	reg := registry395(t, displayName)
	opts = append(opts, WithChatAuditRepo(repo))
	agent := NewAgent(chat.NewClient(srv.URL, "k", "m"), nil, nil, nil, reg, opts...)
	var turnID string
	ctx := context.Background()
	res := agent.Process(ctx, Request{
		Messages:        []chat.Message{{Role: "user", Content: "list my projects"}},
		Project:         "p1",
		AllowedProjects: []string{"*"},
	})
	require.NoError(t, res.Err)
	rows, err := repo.List(ctx, persistence.ChatAuditFilter{PageSize: 5})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	turnID = rows[0].ID
	return func() string { return turnID }
}

func registry395(t *testing.T, displayName string) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "s1.md"), []byte("---\nswarmId: \"s1\"\nroles:\n  - name: \"researcher\"\n    runtime:\n      image: \"test:latest\"\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "research.md"), []byte("---\nworkflowId: \"research\"\nentrypoint: \"run\"\nsteps:\n  run:\n    type: \"agent\"\n    prompt: \"do work\"\n    role: \"researcher\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "p1.yaml"), []byte(fmt.Sprintf("projectId: \"p1\"\ndisplayName: %q\nswarmId: \"s1\"\ndefaultWorkflowId: \"research\"\n", displayName)), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

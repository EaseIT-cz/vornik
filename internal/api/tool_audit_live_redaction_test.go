package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// Synthetic secrets. Neither is a real credential; assertions compare markers.
const (
	// apiKeyShaped has the shape of a Google API key (AIza + 35 chars).
	liveRedactAPIKey = "AIzaSyDUMMYKEYFORTESTINGONLY0123456789A"
	// configuredSecret matches only the operator-configured custom pattern
	// below, not any default pattern, so it proves the live path uses the
	// operator's effective corpus rather than a second, built-in one.
	liveRedactConfigured = "acme_tok_Q7wP2xL9mK4vB8nR3tY6zH1c"
)

// The production seam must keep the read-back, or every live tool event takes
// the fail-closed branch and loses its I/O (GitHub #293).
var _ toolAuditStoredLogger = (*auditredact.Repo)(nil)

// liveEventRowRepo stands in for execution_live_events: it keeps the exact
// payload bytes the DB-backed publisher would INSERT.
type liveEventRowRepo struct {
	mu   sync.Mutex
	rows [][]byte
}

func (r *liveEventRowRepo) Append(_ context.Context, _, _ string, payload []byte) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, append([]byte(nil), payload...))
	return int64(len(r.rows)), nil
}

func (r *liveEventRowRepo) ListSince(context.Context, string, int64, int) ([]*persistence.ExecutionLiveEvent, error) {
	return nil, nil
}

func (r *liveEventRowRepo) LatestSeq(context.Context, string) (int64, error) { return 0, nil }

func (r *liveEventRowRepo) DeleteOlderThan(context.Context, time.Time) (int64, error) {
	return 0, nil
}

// teeLiveSub records the payload every Publish carries (what a live
// subscriber, the fleet view, the narrator and the A2A bridge receive), then
// hands it to the real DB-backed publisher, which writes the live_events row.
type teeLiveSub struct {
	inner    livepubsub.Publisher
	payloads []string
}

func (t *teeLiveSub) Subscribe(id string, from int64) (<-chan livepubsub.LiveEvent, func(), error) {
	return t.inner.Subscribe(id, from)
}

func (t *teeLiveSub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	return nil, func() {}, nil
}

func (t *teeLiveSub) Publish(ctx context.Context, executionID, kind string, payload any) int64 {
	raw, _ := json.Marshal(payload)
	t.payloads = append(t.payloads, kind+" "+string(raw))
	return t.inner.Publish(ctx, executionID, kind, payload)
}

// GitHub #293 (2026-10-08): the live tool_call_started / tool_call_finished
// events were built from the raw request after Log, while the redaction seam
// (auditredact) redacted only the copy it stored. The audit row was clean; the
// live event, and the DB-backed publisher's live_events row, carried the
// credential. One redaction must be applied before both sinks.
func TestIngestToolAudit_LiveEventsCarryTheRedactedRow(t *testing.T) {
	detector, err := secrets.NewMultiDetector(secrets.Config{
		Patterns: secrets.EffectivePatterns(nil, []secrets.Pattern{{
			Name:  "acme_internal_token",
			Regex: `acme_tok_[A-Za-z0-9]{24}`,
		}}),
	})
	if err != nil {
		t.Fatalf("build detector: %v", err)
	}
	inner := &capturingToolAuditRepo{}
	seam := auditredact.New(inner, detector, nil, nil, nil, nil)

	rows := &liveEventRowRepo{}
	pub, shutdown, err := livepubsub.NewDBBacked(context.Background(), livepubsub.NewDBBackedConfig{
		Repo:   rows,
		Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("build DB-backed publisher: %v", err)
	}
	defer shutdown()
	sub := &teeLiveSub{inner: pub}

	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(seam), WithLiveSubscriber(sub))

	reqBody, _ := json.Marshal(map[string]any{
		"audit_id":     "a-293",
		"project_id":   "proj-a",
		"execution_id": "exec-293",
		"step_id":      "s1",
		"tool_name":    "run_shell",
		"tool_input":   "curl -H 'x-goog-api-key: " + liveRedactAPIKey + "' -H 'auth: " + liveRedactConfigured + "'",
		"tool_output":  "echoed " + liveRedactAPIKey + " and " + liveRedactConfigured,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}

	// The audit row stays redacted, as before.
	if inner.logged == nil {
		t.Fatal("no audit row stored")
	}
	for _, field := range []string{inner.logged.ToolInput, inner.logged.ToolOutput} {
		assertNoPlantedSecret(t, "audit row", field)
		if !strings.Contains(field, "[REDACTED:") {
			t.Errorf("audit row field lacks a redaction marker: %q", field)
		}
	}

	// Both live events were published and carry the redacted text.
	if len(sub.payloads) != 2 {
		t.Fatalf("want 2 live events (started + finished), got %d: %v", len(sub.payloads), sub.payloads)
	}
	for _, p := range sub.payloads {
		assertNoPlantedSecret(t, "live event", p)
		if !strings.Contains(p, "[REDACTED:") {
			t.Errorf("live event lacks the stored redaction marker (built from something other than the row?): %s", p)
		}
	}

	// And so do the live_events rows the DB-backed publisher wrote.
	rows.mu.Lock()
	defer rows.mu.Unlock()
	if len(rows.rows) != 2 {
		t.Fatalf("want 2 live_events rows, got %d", len(rows.rows))
	}
	for _, r := range rows.rows {
		assertNoPlantedSecret(t, "live_events row", string(r))
	}
}

// opaqueRedactingRepo redacts what it stores but cannot say what it stored: a
// wrapper placed outside the seam. The handler must not fall back to the raw
// request for it (review-20261008-eb1b F1).
type opaqueRedactingRepo struct{ capturingToolAuditRepo }

func (o *opaqueRedactingRepo) Log(ctx context.Context, e *persistence.ToolAuditEntry) error {
	cp := *e
	cp.ToolInput = strings.ReplaceAll(cp.ToolInput, liveRedactAPIKey, "[REDACTED:x]")
	cp.ToolOutput = strings.ReplaceAll(cp.ToolOutput, liveRedactAPIKey, "[REDACTED:x]")
	return o.capturingToolAuditRepo.Log(ctx, &cp)
}

// GitHub #293 (2026-10-08): when the repository cannot return the stored row,
// the handler fails closed: both events still publish (the live view keeps
// showing tool use) but carry no tool input or output.
func TestIngestToolAudit_LiveEventsWithholdIOWhenStoredRowUnknown(t *testing.T) {
	repo := &opaqueRedactingRepo{}
	sub := &teeLiveSub{inner: livepubsub.New(16)}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo), WithLiveSubscriber(sub))

	reqBody, _ := json.Marshal(map[string]any{
		"audit_id": "a2", "project_id": "proj-a", "execution_id": "exec-2",
		"tool_name": "run_shell", "tool_input": "k=" + liveRedactAPIKey, "tool_output": "echo " + liveRedactAPIKey,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(sub.payloads) != 2 {
		t.Fatalf("want 2 live events, got %v", sub.payloads)
	}
	for _, p := range sub.payloads {
		assertNoPlantedSecret(t, "live event", p)
		if strings.Contains(p, "input_json") || strings.Contains(p, "output_json") {
			t.Errorf("live event must withhold tool I/O when the stored row is unknown: %s", p)
		}
	}
	if !strings.Contains(sub.payloads[0], `"run_shell"`) {
		t.Errorf("tool_call_started should still name the tool: %s", sub.payloads[0])
	}
}

func assertNoPlantedSecret(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, liveRedactAPIKey) {
		t.Errorf("%s carries the API-key-shaped secret: %s", where, s)
	}
	if strings.Contains(s, liveRedactConfigured) {
		t.Errorf("%s carries the operator-configured secret: %s", where, s)
	}
}

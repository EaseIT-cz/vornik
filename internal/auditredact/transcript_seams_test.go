package auditredact

// GitHub #395 — the content-store seams (chat-audit retention and redaction
// design §9.2). Unit level; the writer-level regressions live beside the
// writers (dispatcher, api) and run against real SQLite.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/secrets"
)

const (
	tok395 = "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
	aws395 = "AKIAQ3EGUZJ7RLW4XN2P"
)

func det395(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{Patterns: secrets.EffectivePatterns(nil, nil), Allowlist: secrets.DefaultAllowlist()})
	require.NoError(t, err)
	return d
}

func TestFragmentRule(t *testing.T) {
	s := newTranscriptScan(det395(t), false)
	_ = s.text("reply with " + tok395 + " inside") // becomes a known match
	// A prefix ending the string, before the cut's ellipsis.
	got := s.fragments("see https://x.test/" + tok395[:20] + "…")
	require.NotContains(t, got, tok395[:12])
	require.True(t, strings.HasSuffix(got, "…"))
	// A suffix beginning the string.
	got = s.fragments(tok395[15:] + " to call it")
	require.NotContains(t, got, tok395[20:30])
	// The whole match anywhere.
	got = s.fragments("x " + tok395 + " y " + tok395)
	require.NotContains(t, got, tok395)
	// Fragments shorter than 8 bytes are not touched.
	require.Equal(t, "ends ghp_R7m", s.fragments("ends ghp_R7m"))
}

func TestEdgeRunRule(t *testing.T) {
	s := newTranscriptScan(det395(t), false)
	// "/" and "." are in the alphabet, so the run reaches back to the ":".
	require.Equal(t, "url https:[REDACTED:cut_token]…", s.edgeRuns("url https://x.test/AbCdEfGhIjKlMnOpQrSt…", false))
	require.Equal(t, "[REDACTED:cut_token] to call", s.edgeRuns("AbCdEfGhIjKlMnOpQrSt to call", true))
	// Ordinary words at an edge survive; no ellipsis means no cut at the end.
	require.Equal(t, "Internationally…", s.edgeRuns("Internationally…", false))
	require.Equal(t, "AbCdEfGhIjKlMnOpQrSt", s.edgeRuns("AbCdEfGhIjKlMnOpQrSt", false))
	require.Equal(t, 2, s.counts[findingCutToken])
}

func TestMetadataWalk(t *testing.T) {
	s := newTranscriptScan(det395(t), true)
	in := []byte(`{"credentials":{"user":"x"},"list":[{"note":"k ` + aws395 + `"}],"password":"","n":12345678901234567890,"tokens_used":3}`)
	out := metadata(s, in)
	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	require.Equal(t, markerSecretField, m["credentials"], "a secret-named key loses its whole value, container included")
	require.Equal(t, "", m["password"], "an empty value has nothing to hide")
	require.NotContains(t, string(out), aws395)
	require.Contains(t, string(out), "12345678901234567890", "numbers keep their precision")
	// Clean metadata is returned byte-identical.
	clean := []byte(`{"reason":"moved",  "from":"RUNNING"}`)
	require.Equal(t, clean, metadata(newTranscriptScan(det395(t), true), clean))
	// Entropy is not applied to metadata: a long opaque id survives.
	id := []byte(`{"step_id":"Zk19QpRt7vB4Nx72HsWy5cQ8a8Kd93jXqLm2Zk19QpRt7vB4"}`)
	require.Equal(t, id, metadata(newTranscriptScan(det395(t), true), id))
	// Not JSON: redacted as text.
	raw := metadata(newTranscriptScan(det395(t), true), []byte("token "+tok395))
	require.NotContains(t, string(raw), tok395)
}

func TestToolCallsShape(t *testing.T) {
	// Another shape: raw-text redaction, no cut.
	s := newTranscriptScan(det395(t), false)
	require.NotContains(t, toolCalls(s, `{"weird":"`+tok395+`"}`), tok395)
	require.Equal(t, "[]", toolCalls(s, "[]"))
	require.Equal(t, "null", toolCalls(s, "null"))
	// Bypass (nil scan): the dispatcher's shape is still cut.
	long := `[{"name":"t","result":"` + strings.Repeat("r", 3000) + `"}]`
	var calls []toolCallShape
	require.NoError(t, json.Unmarshal([]byte(toolCalls(nil, long)), &calls))
	require.LessOrEqual(t, len(calls[0].Result), ToolFieldLimit)
}

// recordingMessages is the inner task-message repository.
type recordingMessages struct {
	got []*persistence.TaskMessage
}

func (r *recordingMessages) Insert(_ context.Context, m *persistence.TaskMessage) error {
	cp := *m
	cp.ID = "tmsg_1"
	r.got = append(r.got, &cp)
	m.ID = cp.ID
	return nil
}
func (r *recordingMessages) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}
func (r *recordingMessages) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return nil, persistence.ErrNotFound
}
func (r *recordingMessages) MarkCheckpointResolved(context.Context, string, string) error { return nil }

func TestRecordingMessages_MissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "TaskMessageRepository.GetOpenCheckpoint", (&recordingMessages{}).GetOpenCheckpoint)
}

func seamCount(t *testing.T, reg *prometheus.Registry, store, status, reason string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "vornik_redaction_seam_rows_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["store"] == store && labels["status"] == status && labels["reason"] == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestTaskMessages_ScansCopiesBackIDAndCounts(t *testing.T) {
	inner := &recordingMessages{}
	metrics := NewMetrics()
	reg := prometheus.NewRegistry()
	d := NewTaskMessages(inner, det395(t), nil, nil)
	d.SetMetrics(metrics)
	msg := &persistence.TaskMessage{TaskID: "t", Content: "use " + tok395, Metadata: []byte(`{"apiKey":"abc-123-def"}`)}
	require.NoError(t, d.Insert(context.Background(), msg))
	require.Equal(t, "tmsg_1", msg.ID, "callers read the assigned id")
	require.Contains(t, msg.Content, tok395, "the caller's struct is not edited")
	require.NotContains(t, inner.got[0].Content, tok395)
	require.NotContains(t, string(inner.got[0].Metadata), "abc-123-def")
	require.NoError(t, d.Insert(context.Background(), &persistence.TaskMessage{TaskID: "t", Content: "fine"}))
	metrics.Attach(reg, nil) // a write before Attach is drained into the series
	require.Equal(t, 1.0, seamCount(t, reg, StoreTaskMessages, "redacted", ""))
	require.Equal(t, 1.0, seamCount(t, reg, StoreTaskMessages, "scanned", ""))
	tot := metrics.SeamTotals()[StoreTaskMessages]
	require.Equal(t, SeamCount{Total: 2, Scanned: 1, Redacted: 1}, tot)
	_, err := d.GetOpenCheckpoint(context.Background(), "t")
	require.ErrorIs(t, err, persistence.ErrNotFound)
	_, _ = d.List(context.Background(), persistence.TaskMessageFilter{})
	require.NoError(t, d.MarkCheckpointResolved(context.Background(), "t", "c"))
	require.Same(t, persistence.TaskMessageRepository(inner), d.Inner())
}

// The bypass stores content verbatim and counts every write with its reason —
// the census that was missing for every store but tool_audit_log.
func TestContentSeamBypassesCountWithTheirReason(t *testing.T) {
	metrics := NewMetrics()
	reg := prometheus.NewRegistry()
	metrics.Attach(reg, nil)

	tm := NewTaskMessagesBypassed(&recordingMessages{}, ReasonSecretsDisabled, nil)
	tm.SetMetrics(metrics)
	require.Equal(t, ReasonSecretsDisabled, tm.BypassReason())
	require.NoError(t, tm.Insert(context.Background(), &persistence.TaskMessage{Content: tok395}))

	chatInner := &recordingChatRepo{}
	ca := NewChatAuditBypassed(chatInner, ReasonSecretsDisabled, nil)
	ca.SetMetrics(metrics)
	require.Equal(t, ReasonSecretsDisabled, ca.BypassReason())
	require.NoError(t, ca.Insert(context.Background(), &persistence.ChatAuditEntry{UserMessage: strings.Repeat("u", 2000)}))
	_, err := ca.SavePrompt(context.Background(), "prompt")
	require.NoError(t, err)
	require.Len(t, chatInner.entries[0].UserMessage, ChatTextLimit, "the bypass still owns the cut")

	sp := NewStepPromptsBypassed(&recordingPromptRepo{}, ReasonDetectorUnavailable, nil)
	sp.SetMetrics(metrics)
	require.False(t, sp.Scans())
	_, err = sp.Save(context.Background(), persistence.StepPromptSystem, "body")
	require.NoError(t, err)

	require.Equal(t, 1.0, seamCount(t, reg, StoreTaskMessages, "skipped", "secrets_disabled"))
	require.Equal(t, 1.0, seamCount(t, reg, StoreChatAuditLog, "skipped", "secrets_disabled"))
	require.Equal(t, 1.0, seamCount(t, reg, StoreChatSystemPrompts, "skipped", "secrets_disabled"))
	require.Equal(t, 1.0, seamCount(t, reg, StoreStepPrompts, "skipped", "detector_unavailable"))
	require.Equal(t, int64(1), metrics.SeamTotals()[StoreTaskMessages].Skipped)
}

// The signals column is walked as JSON: every string scanned, the fragment
// rule applied with the row's known matches, the edge-run rule on the two cut
// fields; untouched columns keep the writer's bytes.
func TestChatAuditInsert_SignalsScannedAndCut(t *testing.T) {
	inner := &recordingChatRepo{}
	metrics := NewMetrics()
	c := NewChatAudit(inner, det395(t), nil, nil)
	c.SetMetrics(metrics)
	signalsIn := `[{"detector":"url","severity":"warn","claim_type":"url","claim_value":"https://x.test/` +
		tok395[:20] + `…","sentence":"` + tok395[12:] + ` to call","detail":"key ` + aws395 + `","recorded_at":"2026-10-16T00:00:00Z"}]`
	e := &persistence.ChatAuditEntry{ID: "c1", Response: "token " + tok395 + " ok", ToolCallsJSON: "[]", HallucinationSignalsJSON: signalsIn}
	require.NoError(t, c.Insert(context.Background(), e))
	got := inner.entries[0].HallucinationSignalsJSON
	for _, frag := range []string{tok395[:12], tok395[20:32], aws395} {
		require.NotContains(t, got, frag)
	}
	var list []map[string]any
	require.NoError(t, json.Unmarshal([]byte(got), &list))
	require.Equal(t, "url", list[0]["claim_type"])
	require.Equal(t, signalsIn, e.HallucinationSignalsJSON, "the caller's struct is not edited")

	// Clean signals keep their bytes; an undecodable column is redacted as text.
	clean := `[{"detector":"d","claim_value":"fine"}]`
	require.Equal(t, clean, signals(newTranscriptScan(det395(t), false), clean))
	require.NotContains(t, signals(newTranscriptScan(det395(t), false), "not json "+aws395), aws395)
	require.Equal(t, "", signals(newTranscriptScan(det395(t), false), ""))
	require.Equal(t, int64(1), metrics.SeamTotals()[StoreChatAuditLog].Redacted)
}

func TestContentSeamDelegatesAndNilSafety(t *testing.T) {
	ctx := context.Background()
	inner := &recordingChatRepo{}
	c := NewChatAudit(inner, nil, nil, nil)
	require.Same(t, persistence.ChatAuditRepository(inner), c.Inner())
	_, _ = c.List(ctx, persistence.ChatAuditFilter{})
	_, _ = c.GetChatAuditsByTurnIDs(ctx, nil)
	_, err := c.GetByID(ctx, "missing")
	require.ErrorIs(t, err, persistence.ErrNotFound)
	h, err := c.SavePrompt(ctx, "p")
	require.NoError(t, err)
	body, err := c.GetPrompt(ctx, h)
	require.NoError(t, err)
	require.Equal(t, "p", body)

	var nilChat *ChatAudit
	require.Equal(t, ReasonNone, nilChat.BypassReason())
	var nilTM *TaskMessages
	require.Equal(t, ReasonNone, nilTM.BypassReason())
	var nilSP *StepPrompts
	require.Equal(t, ReasonNone, nilSP.BypassReason())
	require.False(t, nilSP.Scans())

	prompts := &recordingPromptRepo{}
	sp := NewStepPrompts(prompts, det395(t), nil, nil)
	require.Same(t, persistence.StepPromptRepository(prompts), sp.Inner())
	require.True(t, sp.Scans())
	h, err = sp.Save(ctx, persistence.StepPromptSystem, "key "+aws395)
	require.NoError(t, err)
	got, err := sp.Get(ctx, h)
	require.NoError(t, err)
	require.NotContains(t, got.Body, aws395)
	_, _ = sp.PruneUnreferenced(ctx)

	tm := NewTaskMessages(&recordingMessages{}, nil, nil, nil)
	require.NoError(t, tm.Insert(ctx, &persistence.TaskMessage{Content: "x"}))
	var nilMetrics *Metrics
	require.Empty(t, nilMetrics.SeamTotals())
}

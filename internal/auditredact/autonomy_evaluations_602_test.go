package auditredact

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #602 (design 11, amendment 2026-11-20, B4): an observe-mode
// WOULD_START row stores the prompt the tick would have submitted, which is
// model text. autonomy_evaluations had no redaction seam (section A), so the
// change adds one: the proposed prompt and the reason are scanned before
// they are stored.

type captureEvaluations struct {
	rows []persistence.AutonomyEvaluation
}

func (c *captureEvaluations) Record(_ context.Context, e *persistence.AutonomyEvaluation) error {
	c.rows = append(c.rows, *e)
	return nil
}
func (c *captureEvaluations) List(context.Context, persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	return nil, nil
}
func (c *captureEvaluations) CountByOutcome(context.Context, string, time.Time, time.Time) (map[string]int64, error) {
	return nil, nil
}
func (c *captureEvaluations) LatestByProject(context.Context) (map[string]*persistence.AutonomyEvaluation, error) {
	return nil, nil
}

func TestAutonomyEvaluations602_RedactsTheProposedPromptAndReason(t *testing.T) {
	inner := &captureEvaluations{}
	audit := &recordingAudit{}
	repo := NewAutonomyEvaluations(inner, detector(t), audit, nil)
	in := &persistence.AutonomyEvaluation{
		ID: "e1", ProjectID: "p1", Outcome: persistence.AutonomyOutcomeWouldStart,
		Reason:         "observe mode; key " + syntheticKey,
		ProposedPrompt: "Rotate the deploy key " + syntheticKey + " and report",
		PromptHash:     "h1",
	}
	if err := repo.Record(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(inner.rows) != 1 {
		t.Fatalf("stored %d rows, want 1", len(inner.rows))
	}
	got := inner.rows[0]
	for name, v := range map[string]string{"proposed prompt": got.ProposedPrompt, "reason": got.Reason} {
		if strings.Contains(v, syntheticKey) || !strings.Contains(v, "[REDACTED") {
			t.Fatalf("stored %s %q: the secret must be replaced by its marker", name, v)
		}
	}
	if got.ID != "e1" || got.PromptHash != "h1" || got.Outcome != persistence.AutonomyOutcomeWouldStart {
		t.Fatalf("daemon-set fields changed: %+v", got)
	}
	if !strings.Contains(in.ProposedPrompt, syntheticKey) {
		t.Fatal("the caller's struct must not be edited")
	}
	if len(audit.events) == 0 {
		t.Fatal("the redaction must be recorded in the redaction audit")
	}

	_ = repo.Record(context.Background(), &persistence.AutonomyEvaluation{ID: "e2", ProposedPrompt: "Summarise the open P1 issues"})
	if inner.rows[1].ProposedPrompt != "Summarise the open P1 issues" {
		t.Fatalf("a clean prompt changed: %q", inner.rows[1].ProposedPrompt)
	}

	bypass := NewAutonomyEvaluationsBypassed(inner, ReasonSecretsDisabled, nil)
	_ = bypass.Record(context.Background(), &persistence.AutonomyEvaluation{ID: "e3", ProposedPrompt: "k " + syntheticKey})
	if !strings.Contains(inner.rows[2].ProposedPrompt, syntheticKey) {
		t.Fatal("a bypassed seam stores unscanned (and counts it)")
	}
	if bypass.BypassReason() != ReasonSecretsDisabled || repo.BypassReason() != ReasonNone {
		t.Fatal("the bypass reason must be reported")
	}
	if repo.Inner() != inner {
		t.Fatal("Inner must return the undecorated repository")
	}
}

func TestAutonomyEvaluations602_IsAListedSeamStore(t *testing.T) {
	for _, s := range SeamStores {
		if s == StoreAutonomyEvaluations {
			return
		}
	}
	t.Fatalf("SeamStores %v must list %q", SeamStores, StoreAutonomyEvaluations)
}

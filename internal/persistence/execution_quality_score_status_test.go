package persistence

import (
	"errors"
	"testing"
)

// The durable row learns `unscorable` (agent-quality-benchmark design,
// amendment 2026-09-26). Incident: the slow-hardware bench arm, where the
// validator refused the scorer's `unscorable` verdict and the reconciler
// retried the same execution every 30 s, forever (197 warnings in 3 h).

func qualityRow(status string, score *float64) *ExecutionQualityScore {
	return &ExecutionQualityScore{ProjectID: "p", TaskID: "t", ExecutionID: "e", WorkflowID: "w", Status: status, Score: score}
}

func TestValidateExecutionQualityScore_Unscorable(t *testing.T) {
	if err := ValidateExecutionQualityScore(qualityRow("unscorable", nil)); err != nil {
		t.Fatalf("unscorable with a NULL score is a valid durable row: %v", err)
	}
	zero := 0.0
	if err := ValidateExecutionQualityScore(qualityRow("unscorable", &zero)); !errors.Is(err, ErrInvalidQualityScore) {
		t.Fatalf("unscorable is not a measurement and must not carry a number, got %v", err)
	}
}

func TestValidateExecutionQualityScore_UnknownStatusIsAStructuralReject(t *testing.T) {
	zero := 0.0
	err := ValidateExecutionQualityScore(qualityRow("bogus", &zero))
	if !errors.Is(err, ErrInvalidQualityScore) {
		t.Fatalf("a status outside the list is a structural reject, got %v", err)
	}
}

func TestExecutionQualityStatuses_AgreesWithTheUnmeasuredGroup(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range ExecutionQualityStatuses {
		seen[s] = true
		var score *float64
		if !ExecutionQualityScoreUnmeasured(s) {
			v := 0.5
			score = &v
		}
		if err := ValidateExecutionQualityScore(qualityRow(s, score)); err != nil {
			t.Errorf("listed status %q with its correct score shape must validate: %v", s, err)
		}
	}
	for _, s := range []string{"scored", "missing_contract", "invalid_evidence", "not_applicable", "unscorable"} {
		if !seen[s] {
			t.Errorf("%q missing from ExecutionQualityStatuses", s)
		}
	}
	if !ExecutionQualityScoreUnmeasured("not_applicable") || !ExecutionQualityScoreUnmeasured("unscorable") || ExecutionQualityScoreUnmeasured("invalid_evidence") {
		t.Error("NULL score is exactly not_applicable and unscorable")
	}
}

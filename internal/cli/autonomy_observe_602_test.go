package cli

// GitHub #602 (design 11, amendment 2026-11-20, B5): `vornikctl autonomy
// evaluations --outcome WOULD_START` shows what observe mode would have
// started. The table's last column carries the proposal, cut to 80
// characters; --json carries it whole (the API serialises the struct).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRunAutonomyEvaluations_ShowsTheProposal_602(t *testing.T) {
	long := "Summarise the open issues labelled P1 and draft a reply to each reporter, " + strings.Repeat("then more detail ", 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"evaluations": []map[string]any{
				{"id": "e1", "project_id": "family", "outcome": "WOULD_START", "reason": "observe mode: every gate passed; nothing was started", "proposed_prompt": long},
				{"id": "e2", "project_id": "family", "outcome": "NO_ACTION", "reason": "nothing to do"},
			},
			"total": 2,
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	autonomyCov_reset()
	autonomyProject, autonomyOutcome = "family", "WOULD_START"
	out, err := captureStdoutFunc(t, func() error { return runAutonomyEvaluations(autonomyEvaluationsCmd, nil) })
	if err != nil {
		t.Fatalf("runAutonomyEvaluations: %v", err)
	}
	if !strings.Contains(out, "would start: Summarise the open issues labelled P1") {
		t.Errorf("a WOULD_START row must show its proposal:\n%s", out)
	}
	if strings.Contains(out, "then more detail then more detail then more detail then more detail") {
		t.Errorf("the proposal must be cut to 80 characters in the table:\n%s", out)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("other rows keep their reason:\n%s", out)
	}
	if !strings.Contains(autonomyEvaluationsCmd.Long, "WOULD_START") {
		t.Error("the command's help must list the WOULD_START outcome")
	}
}

// Review f6a3 finding 2: the cut is by runes, so a multi-byte proposal is
// never split inside a character.
func TestEvalReasonCell_CutsByRunes_602(t *testing.T) {
	cell := evalReasonCell(autonomyEvalRow{Outcome: "WOULD_START", ProposedPrompt: strings.Repeat("ž", 120)})
	if !utf8.ValidString(cell) {
		t.Fatalf("the cut split a character: %q", cell)
	}
	if n := utf8.RuneCountInString(cell); n != 80 {
		t.Fatalf("cell has %d runes, want 80", n)
	}
}

package api

import "testing"

// GitHub #704 (design 2026-08-20 §13.5 fifth round, §13.16): on Claude Code
// the recall hint was followed in 3 of 3 runs with an imperative naming the
// exact tool, against 0 of 3 with the advisory text. The catalogue ships the
// measured text, at version 2.
func TestCompanionHints_RecallHintTakesTheMeasuredWording_704(t *testing.T) {
	cat, err := CompanionHints()
	if err != nil {
		t.Fatal(err)
	}
	const want = "vornik-companion: ACTION REQUIRED as your next step. Call {recall} with query set to the failing test's name and its error message, then read the hits before you change code."
	for _, h := range cat.Hints {
		if h.ID != "test_failed.recall" {
			continue
		}
		if h.Version != 2 {
			t.Errorf("version %d, want 2 (bump on every wording change)", h.Version)
		}
		if h.Text != want {
			t.Errorf("text\n got %q\nwant %q", h.Text, want)
		}
		return
	}
	t.Fatal("test_failed.recall is missing from the catalogue")
}

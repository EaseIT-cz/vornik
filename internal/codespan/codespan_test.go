package codespan

import "testing"

// GitHub #612: one code-region implementation for the hallucination rules
// and the agent loop's write check.
func TestStripAndSpansAgree_612(t *testing.T) {
	text := "a `x` b\n```\ncode\n```\nc"
	if got := Strip(text); got != "a  b\n\nc" {
		t.Fatalf("Strip = %q", got)
	}
	spans := Spans(text)
	for at, want := range map[int]bool{0: false, 2: true, 4: true, 5: false, 8: true, 18: true, len(text) - 1: false} {
		if Inside(spans, at) != want {
			t.Errorf("Inside(%d) = %v, want %v (spans %v)", at, !want, want, spans)
		}
	}
	// An unclosed fence covers only its own backticks: the text after it
	// stays outside every span.
	open := "```\nopen fence, no close"
	if s := Spans(open); Inside(s, 4) || Inside(s, len(open)-1) {
		t.Errorf("unclosed fence spans = %v cover the text after it", s)
	}
}

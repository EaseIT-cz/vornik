package persistence

import "testing"

// GitHub #449: the one LIKE escaper, the ASCII-only fold and the JSON element
// needle that both drivers' text matches are built from.
func TestLikePatternHelpers_449(t *testing.T) {
	for in, want := range map[string]string{
		"plain": "plain", "%": `\%`, "_": `\_`, `\`: `\\`, `a\%_b`: `a\\\%\_b`,
	} {
		if got := EscapeLike(in); got != want {
			t.Errorf("EscapeLike(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"JOHN Doe": "john doe", "JÜRGEN": "jÜrgen", "İI": "İi", "": "",
	} {
		if got := ASCIILower(in); got != want {
			t.Errorf("ASCIILower(%q) = %q, want %q", in, got, want)
		}
	}
	if got, want := LikeContainsFold("A_%"), `%a\_\%%`; got != want {
		t.Errorf("LikeContainsFold = %q, want %q", got, want)
	}
	for in, want := range map[string]string{
		"researcher": `"researcher"`, `q"x`: `"q\"x"`, `b\s`: `"b\\s"`, "<a>": "\"\\u003ca\\u003e\"",
	} {
		if got := JSONElementNeedle(in); got != want {
			t.Errorf("JSONElementNeedle(%q) = %q, want %q", in, got, want)
		}
	}
}

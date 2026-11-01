package textutil

import "testing"

// GitHub #344 (security review of cc464bb70): the erasure review gate printed
// transcript text raw, so an ESC sequence in a chat message could hide or
// rewrite what the operator approved.
func TestEscapeControl(t *testing.T) {
	cases := map[string]string{
		"plain text\nline two\ttab": "plain text\nline two\ttab",
		"red \x1b[31mhidden\x1b[0m": `red \u001b[31mhidden\u001b[0m`,
		"back\rover":                `back\u000dover`,
		"c1 \u009b2J":               `c1 \u009b2J`,
		"nul\x00del\x7f":            `nul\u0000del\u007f`,
		"čeština ✓":                 "čeština ✓",
	}
	for in, want := range cases {
		if got := EscapeControl(in); got != want {
			t.Errorf("EscapeControl(%q) = %q, want %q", in, got, want)
		}
	}
}

// Security review of 84d47464b: bidi overrides and other format characters
// reorder or hide displayed text (Trojan Source), and a raw invalid byte such
// as 0x9b (an 8-bit CSI) passed the fast path because it decodes as U+FFFD.
func TestEscapeControl_FormatCharactersAndInvalidBytes(t *testing.T) {
	cases := map[string]string{
		"approve \u202edelete\u202c": `approve \u202edelete\u202c`,
		"zero\u200bwidth":            `zero\u200bwidth`,
		"line\u2028sep":              `line\u2028sep`,
		"bom\ufeff":                  `bom\ufeff`,
		"raw \x9b2J byte":            `raw \x9b2J byte`,
		"cut \xe2\x82":               `cut \xe2\x82`,
		"nbsp\u00a0x":                `nbsp\u00a0x`,
		"a real \ufffd stays":        "a real \ufffd stays",
	}
	for in, want := range cases {
		if got := EscapeControl(in); got != want {
			t.Errorf("EscapeControl(%q) = %q, want %q", in, got, want)
		}
	}
}

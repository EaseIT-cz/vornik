package textutil

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EscapeControl renders every rune a terminal does not print as itself,
// except newline and tab, as a visible escape: C0/C1 controls and DEL, format
// characters such as the bidi overrides and zero-width spaces, line and
// paragraph separators, and any other non-printable rune, as \uXXXX; and
// every byte that is not valid UTF-8 as \xNN. So untrusted text printed to an
// operator's terminal cannot move the cursor, recolour, clear, reorder or
// hide what is shown.
//
// Escape rather than strip where a human decides on what they read: the
// erasure review gate (GitHub #344) shows transcript text another person
// wrote, and an attempt to hide part of it must stay visible. The strippers
// elsewhere (api.stripControlRunes, conversation.stripControl) serve text
// that is stored or published. Hardened after the security review of
// 84d47464b: bidi controls and invalid bytes (a raw 0x9b is an 8-bit CSI)
// passed the first version.
func EscapeControl(s string) string {
	if !needsEscape(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case escapedRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func needsEscape(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || escapedRune(r) {
			return true
		}
		i += size
	}
	return false
}

// escapedRune: everything unicode.IsPrint refuses (controls, format
// characters, separators other than the ASCII space, unassigned runes),
// except newline and tab.
func escapedRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return !unicode.IsPrint(r)
}

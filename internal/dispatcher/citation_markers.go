package dispatcher

import (
	"regexp"
	"slices"
	"strings"

	"vornik.io/vornik/internal/chat"
)

// citationMarkerRE matches any bracketed marker that opens with one of the
// three citation phrases the operator-profile footer defines, plus the
// spaces or tabs after it. Case-insensitive, so "[From your notes: N/A]"
// is caught too.
var citationMarkerRE = regexp.MustCompile(`(?i)\[(?:from your notes|from your profile|overriding profile)[^\]\n]*\][ \t]*`)

var (
	profileCitationRE    = regexp.MustCompile(`^\[from your profile: ([^\]\s]+)\]$`)
	overridingCitationRE = regexp.MustCompile(`^\[overriding profile: ([^\]\s]+)(?:\]| [—–-][^\]]*\])$`)
)

// sanitizeCitationMarkers removes every citation marker that is not one of
// the well-formed shapes the footer defines: "[from your notes]",
// "[from your profile: <known key>]", or "[overriding profile: <known key>…]".
//
// It is an allowlist, not a list of bad phrasings: the model invented
// "[from your notes: not applicable.]" and then copied it from its own
// earlier replies on every turn (2026-09-28; operator-profile-design.md,
// "Malformed citation markers are removed"), and a list of wordings to
// strip would miss the next variant. Text with no removed marker is
// returned unchanged.
func sanitizeCitationMarkers(s string) string {
	lower := strings.ToLower(s)
	if !strings.Contains(lower, "[from your") && !strings.Contains(lower, "[overriding profile") {
		return s
	}
	removed := false
	out := citationMarkerRE.ReplaceAllStringFunc(s, func(m string) string {
		if wellFormedCitation(strings.TrimRight(m, " \t")) {
			return m
		}
		removed = true
		return ""
	})
	if !removed {
		return s
	}
	// A marker removed at the end of a line leaves the space before it.
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// wellFormedCitation: operatorProfileKnownKeys is the only authority on keys;
// the patterns only fix the marker's shape (review 2026-09-28).
func wellFormedCitation(marker string) bool {
	if marker == "[from your notes]" {
		return true
	}
	if m := profileCitationRE.FindStringSubmatch(marker); m != nil {
		return slices.Contains(operatorProfileKnownKeys, m[1])
	}
	if m := overridingCitationRE.FindStringSubmatch(marker); m != nil {
		return slices.Contains(operatorProfileKnownKeys, m[1])
	}
	return false
}

// sanitizeHistoryCitations removes malformed markers from the assistant turns
// of a history copy, so markers already stored stop seeding new ones. A user's
// own words are never altered. msgs is the caller's local copy.
func sanitizeHistoryCitations(msgs []chat.Message) {
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			msgs[i].Content = sanitizeCitationMarkers(msgs[i].Content)
		}
	}
}

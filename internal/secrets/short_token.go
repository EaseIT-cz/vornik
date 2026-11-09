package secrets

import (
	"regexp"
	"sort"
)

// FindingTypeEntropyShort is a random-looking 24 to 39 character token with no
// label, caught at the memory checkpoint only (design 22, amendment 2026-11-08,
// GitHub #438). It is a heuristic type, as entropy is.
const FindingTypeEntropyShort = "entropy_short"

const (
	shortTokenMinLen   = 24
	shortTokenMaxLen   = 39 // 40 and longer are the entropy pass's
	shortTokenMinBits  = 4.0
	shortTokenMinClass = 2 // upper, lower and digits, at least this many each
)

var alnumRunRe = regexp.MustCompile(`[A-Za-z0-9]+`)

// joinsIdentifier reports whether b continues an identifier or a base64
// value, so a run touching it is part of something longer.
func joinsIdentifier(b byte) bool {
	return b == '_' || b == '-' || b == '+' || b == '/' || b == '='
}

// ShortTokenFindings returns the 24 to 39 character alphanumeric runs that
// look like a random credential: not adjoining _ - + / =, with at least two
// upper-case letters, two lower-case letters and two digits, and at least 4.0
// bits per character. A Google Places ID (ChIJ…) is a public identifier and is
// skipped.
func ShortTokenFindings(text []byte) []Finding {
	var out []Finding
	for _, m := range alnumRunRe.FindAllIndex(text, -1) {
		start, end := m[0], m[1]
		if n := end - start; n < shortTokenMinLen || n > shortTokenMaxLen {
			continue
		}
		if (start > 0 && joinsIdentifier(text[start-1])) || (end < len(text) && joinsIdentifier(text[end])) {
			continue
		}
		tok := text[start:end]
		if len(tok) >= 4 && string(tok[:4]) == "ChIJ" {
			continue
		}
		var up, lo, dg int
		for _, c := range tok {
			switch {
			case c >= 'A' && c <= 'Z':
				up++
			case c >= 'a' && c <= 'z':
				lo++
			default:
				dg++
			}
		}
		if up < shortTokenMinClass || lo < shortTokenMinClass || dg < shortTokenMinClass {
			continue
		}
		if shannonEntropy(tok) < shortTokenMinBits {
			continue
		}
		out = append(out, Finding{Type: FindingTypeEntropyShort, Match: string(tok), Start: start, End: end})
	}
	return out
}

// MemoryFindings is what the memory checkpoint scans with: d's findings plus
// the short-token findings no detector finding already covers. Both memory
// writers (the secret_scan gate and the indexer) call it, so the rule is one
// implementation. A nil d scans with the short-token rule alone.
func MemoryFindings(d Detector, text []byte) []Finding {
	var findings []Finding
	if d != nil {
		findings = d.Scan(text)
	}
	covered := spanIndex(findings)
	for _, f := range ShortTokenFindings(text) {
		if !covered.overlaps(f.Start, f.End) {
			findings = append(findings, f)
		}
	}
	// Redact walks findings left to right and skips one that starts before
	// its cursor, so the merged list must be in offset order.
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Start < findings[j].Start })
	return findings
}

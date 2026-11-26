package architecture

// GitHub #888 (testing design 19, §16.2 item 1): the law tests that run a
// regex over the whole non-test tree were the main reason this package hit
// the 10-minute -race timeout on the CI VM. Go's regexp skips ahead quickly
// only when a pattern starts with one literal; an alternation steps through
// every byte, and the race detector instruments that loop (adminClassResolverRe
// took 1.17 s plain, 21.64 s under -race, measured 2026-11-23). A file that
// contains none of the literals every match must include cannot match, so it
// is skipped before the regex runs.
//
// The literals are derived from the parsed regex, never written by hand, so a
// regex edit cannot leave a stale pre-filter that hides a match. A pattern
// the derivation cannot cover (a branch with no literal, a case-insensitive
// literal) gets no pre-filter and runs as before.

import (
	"bytes"
	"regexp"
	"regexp/syntax"
	"sync"
)

var (
	prefilterMu    sync.Mutex
	prefilterCache = map[string][][]byte{}
)

// regexMayMatch reports whether b can contain a match of re: false only when b
// holds none of the literals every match of re must include.
func regexMayMatch(re *regexp.Regexp, b []byte) bool {
	lits := requiredLiterals(re)
	if lits == nil {
		return true
	}
	for _, l := range lits {
		if bytes.Contains(b, l) {
			return true
		}
	}
	return false
}

// requiredLiterals returns literals such that every match of re contains at
// least one of them, or nil when it cannot tell.
func requiredLiterals(re *regexp.Regexp) [][]byte {
	key := re.String()
	prefilterMu.Lock()
	defer prefilterMu.Unlock()
	if lits, ok := prefilterCache[key]; ok {
		return lits
	}
	var lits [][]byte
	if parsed, err := syntax.Parse(key, syntax.Perl); err == nil {
		lits = anyOfLiterals(parsed.Simplify())
	}
	prefilterCache[key] = lits
	return lits
}

// anyOfLiterals: every string re matches contains at least one returned
// literal; nil when no such set is known.
func anyOfLiterals(re *syntax.Regexp) [][]byte {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 || len(re.Rune) == 0 {
			return nil
		}
		return [][]byte{[]byte(string(re.Rune))}
	case syntax.OpCapture, syntax.OpPlus:
		return anyOfLiterals(re.Sub[0])
	case syntax.OpAlternate:
		var all [][]byte
		for _, sub := range re.Sub {
			l := anyOfLiterals(sub)
			if l == nil {
				return nil // one branch can match without a known literal
			}
			all = append(all, l...)
		}
		return all
	case syntax.OpConcat:
		// Every element of a concatenation is part of every match: the
		// longest single literal is the strongest filter, else the first
		// element that yields a set.
		var best []byte
		for _, sub := range re.Sub {
			if sub.Op == syntax.OpLiteral && sub.Flags&syntax.FoldCase == 0 && len(string(sub.Rune)) > len(best) {
				best = []byte(string(sub.Rune))
			}
		}
		if best != nil {
			return [][]byte{best}
		}
		for _, sub := range re.Sub {
			if l := anyOfLiterals(sub); l != nil {
				return l
			}
		}
	}
	return nil
}

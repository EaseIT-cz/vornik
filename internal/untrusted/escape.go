package untrusted

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// lookalikes maps runes a model reads as "<" or "/" onto ASCII, applied
// after NFKC folding and lower-casing. Letters need no table: the name match
// treats every non-ASCII rune as a wildcard for the letter at its position
// (security review 2026-10-21: a hand-written letter table is a denylist
// that reports clean about every lookalike it does not list). Brackets are
// still enumerated: these plus the NFKC forms are what a model reads as a
// tag opening. Design §2.2.
var lookalikes = map[rune]rune{
	// brackets (Unicode confusables of '<' that NFKC leaves alone)
	'‹': '<', '〈': '<', '⟨': '<', '❮': '<', '❬': '<', '❰': '<', '⧼': '<', '˂': '<', 'ᐸ': '<',
	'ᚲ': '<', '\U0001D236': '<',
	// slashes
	'∕': '/', '⁄': '/', '⧸': '/', '╱': '/', '\\': '/',
}

// lessThanRunes is every non-ASCII rune whose fold is '<': the lookalikes
// keys mapped to '<', the runes NFKC folds to '<' (U+FE64, U+FF1C) and the
// runes NFKC folds onto a lookalike (U+2329 and U+FE3F to U+3008).
// escapeMarkers skips any other non-ASCII rune without folding it.
// TestLessThanRunesComplete folds every rune in Unicode and fails if this
// set misses one.
var lessThanRunes = buildLessThanRunes()

func buildLessThanRunes() map[rune]bool {
	m := map[rune]bool{'﹤': true, '＜': true, '〈': true, '︿': true}
	for k, v := range lookalikes {
		if v == '<' {
			m[k] = true
		}
	}
	return m
}

// invisible reports runes a model does not see as text: format characters
// (zero-width spaces and joiners, word joiner, BOM, soft hyphen, bidi
// controls) and combining or enclosing marks (incl. variation selectors).
//
// Control characters (Cc) other than whitespace count too: a tokeniser
// drops or never shows NUL, BEL or ESC, so "<\x00/untrusted_content>" reads
// as the marker (design review round 1, finding 1).
func invisible(r rune) bool {
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) ||
		(unicode.Is(unicode.Cc, r) && !unicode.IsSpace(r))
}

// escapeSeq decodes, at s[pos:], an encoding a model may read as the
// character it names: an HTML numeric character reference ("&#60;",
// "&#x3c;", any number of digits, the ';' optional), a JSON/C escape
// ("<", "\U0000003c", "\x3c") or a URL escape ("%3C", ASCII only).
// A reference too large to name a character is suspicious and decodes as
// '<' (security review 2026-10-21: a digit cap let "&#0000000060;" pass).
// Named references are not decoded: "&lt;" is the inert form this package
// emits and the Prelude explains.
func escapeSeq(s string, pos int) (r rune, size int, ok bool) {
	rest := s[pos:]
	switch {
	case strings.HasPrefix(rest, "&#x"), strings.HasPrefix(rest, "&#X"):
		return charRef(s, pos, pos+3, 16)
	case strings.HasPrefix(rest, "&#"):
		return charRef(s, pos, pos+2, 10)
	case strings.HasPrefix(rest, `\u`):
		return fixedHex(s, pos, 2, 4)
	case strings.HasPrefix(rest, `\U`):
		return fixedHex(s, pos, 2, 8)
	case strings.HasPrefix(rest, `\x`):
		return fixedHex(s, pos, 2, 2)
	case strings.HasPrefix(rest, "%"):
		r, n, ok := fixedHex(s, pos, 1, 2)
		if ok && r >= utf8.RuneSelf {
			return 0, 0, false
		}
		return r, n, ok
	}
	return 0, 0, false
}

// isIntroducer reports the characters that begin an escape sequence.
func isIntroducer(r rune) bool { return r == '&' || r == '\\' || r == '%' }

// maxEscapeLayers bounds how many encodings decodeLayers peels off one
// character ("%2526%2523..." can nest without end).
const maxEscapeLayers = 4

// decodeLayers decodes an escape sequence at s[pos:] and, while the result
// is itself an escape introducer, decodes it again together with the text
// that follows: "&amp;#60;", "&#38;#60;", "%26#60;" and "%253C" all read as
// '<' to a model that decodes twice (security review 2026-10-21, finding
// 5). "&amp;" counts as an introducer only when what follows decodes; a
// plain "&amp;" stays the single character '&'.
func decodeLayers(s string, pos int) (rune, int, bool) {
	r, n, ok := escapeSeq(s, pos)
	if !ok && strings.HasPrefix(s[pos:], "&amp;") {
		r, n, ok = '&', len("&amp;"), true
	}
	if !ok {
		return 0, 0, false
	}
	for layer := 1; isIntroducer(r) && pos+n < len(s); layer++ {
		// Decode the next layer in place, as if the decoded introducer
		// stood just before s[pos+n]; nothing is copied, so a long run of
		// introducers stays linear and a padded reference such as
		// "&amp;#000…0060;" is read in full.
		r2, n2, ok2 := escapeAfter(r, s, pos+n)
		if !ok2 || n2 <= 1 {
			break
		}
		r, n = r2, n+n2-1
		if layer >= maxEscapeLayers {
			// Encoded past the cap: suspicious, so fail closed. Peel the
			// remaining layers only to find where the sequence ends (each
			// consumes bytes, so this stays linear) and read the whole of it
			// as '<', like an over-range numeric reference (security review
			// 2026-10-21: the cap returned the partial decode as clean, so
			// one more layer hid a '<').
			for isIntroducer(r) && pos+n < len(s) {
				r3, n3, ok3 := escapeAfter(r, s, pos+n)
				if !ok3 || n3 <= 1 {
					break
				}
				r, n = r3, n+n3-1
			}
			return '<', n, true
		}
	}
	if r == '&' && strings.HasPrefix(s[pos:], "&amp;") && n == len("&amp;") {
		// "&amp;" that led nowhere: the plain character, one byte.
		return 0, 0, false
	}
	return r, n, true
}

// escapeAfter decodes an escape sequence whose introducer intro was itself
// decoded from an earlier layer and whose remaining bytes start at s[j].
// The returned size counts the introducer as one byte.
func escapeAfter(intro rune, s string, j int) (rune, int, bool) {
	rest := s[j:]
	switch intro {
	case '&':
		switch {
		case strings.HasPrefix(rest, "#x"), strings.HasPrefix(rest, "#X"):
			return charRef(s, j-1, j+2, 16)
		case strings.HasPrefix(rest, "#"):
			return charRef(s, j-1, j+1, 10)
		case strings.HasPrefix(rest, "amp;"):
			return '&', len("&amp;"), true
		}
	case '\\':
		switch {
		case strings.HasPrefix(rest, "u"):
			return fixedHex(s, j-1, 2, 4)
		case strings.HasPrefix(rest, "U"):
			return fixedHex(s, j-1, 2, 8)
		case strings.HasPrefix(rest, "x"):
			return fixedHex(s, j-1, 2, 2)
		}
	case '%':
		if r, n, ok := fixedHex(s, j-1, 1, 2); ok && r < utf8.RuneSelf {
			return r, n, true
		}
	}
	return 0, 0, false
}

// charRef reads every digit from start in the given base.
func charRef(s string, pos, start, base int) (rune, int, bool) {
	i := start
	var v int64
	over := false
	for i < len(s) {
		d := hexDigit(s[i])
		if d < 0 || d >= base {
			break
		}
		if !over {
			v = v*int64(base) + int64(d)
			over = v > unicode.MaxRune
		}
		i++
	}
	if i == start {
		return 0, 0, false
	}
	if i < len(s) && s[i] == ';' {
		i++
	}
	if over {
		return '<', i - pos, true
	}
	return rune(v), i - pos, true
}

// fixedHex reads exactly n hex digits after a prefix of plen bytes.
func fixedHex(s string, pos, plen, n int) (rune, int, bool) {
	start := pos + plen
	if start+n > len(s) {
		return 0, 0, false
	}
	var v int64
	for i := start; i < start+n; i++ {
		d := hexDigit(s[i])
		if d < 0 {
			return 0, 0, false
		}
		v = v*16 + int64(d)
	}
	if v > unicode.MaxRune {
		return '<', plen + n, true
	}
	return rune(v), plen + n, true
}

func hexDigit(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// folder turns one rune of the original text into the runes a model sees
// for it: the decoded character for an escape sequence, nothing for an
// invisible rune, NFKC folding for a non-ASCII rune, then lower-casing and
// the lookalikes map. Invalid UTF-8 becomes U+FFFD, which matches nothing.
// The scratch buffers are reused across calls.
type folder struct {
	enc  [utf8.UTFMax]byte
	nfkc []byte
	out  []rune
}

func (f *folder) fold(s string, pos int) (runes []rune, size int) {
	f.out = f.out[:0]
	if c := s[pos]; c < utf8.RuneSelf {
		if c == '&' || c == '\\' || c == '%' {
			if r, n, ok := decodeLayers(s, pos); ok {
				return f.foldDecoded(r), n
			}
		}
		if invisible(rune(c)) {
			return f.out, 1
		}
		f.out = append(f.out, foldRune(rune(c)))
		return f.out, 1
	}
	r, size := utf8.DecodeRuneInString(s[pos:])
	if r == utf8.RuneError && size == 1 {
		f.out = append(f.out, utf8.RuneError)
		return f.out, 1
	}
	return f.foldNonASCII(r), size
}

// foldDecoded folds a rune named by an escape sequence the way fold folds a
// literal one.
func (f *folder) foldDecoded(r rune) []rune {
	if r < utf8.RuneSelf {
		if !invisible(r) {
			f.out = append(f.out, foldRune(r))
		}
		return f.out
	}
	if !utf8.ValidRune(r) {
		f.out = append(f.out, utf8.RuneError)
		return f.out
	}
	return f.foldNonASCII(r)
}

func (f *folder) foldNonASCII(r rune) []rune {
	if invisible(r) {
		return f.out
	}
	n := utf8.EncodeRune(f.enc[:], r)
	if norm.NFKC.IsNormal(f.enc[:n]) {
		f.out = append(f.out, foldRune(r))
		return f.out
	}
	f.nfkc = norm.NFKC.Append(f.nfkc[:0], f.enc[:n]...)
	for _, x := range string(f.nfkc) {
		if !invisible(x) {
			f.out = append(f.out, foldRune(x))
		}
	}
	return f.out
}

func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		if r == '\\' {
			return '/'
		}
		return r
	}
	r = unicode.ToLower(r)
	if m, ok := lookalikes[r]; ok {
		return m
	}
	return r
}

// The marker name as a pattern over the model view, after the '<':
//
//	sep* u n t r u s t e d sep* c o n t e n t
//
// sep is any rune that is neither a letter nor a digit (whitespace, '/',
// '_', '-', ':', '>', any punctuation or symbol). A letter position matches
// its ASCII letter or ANY non-ASCII rune that is not whitespace, so a
// lookalike spelling needs no table. A non-ASCII symbol may be a separator
// or a lookalike letter; the match follows both.
const markerName = "untrusted" + "content"

// Pattern states: 0 is the separator run before the name, 1..9 the letters
// of "untrusted", 10 the separator run between the words, 11..17 the letters
// of "content", 18 a complete match.
const (
	stLead    = 0
	stMid     = 10
	stMatched = 18
)

// isSep and isWild both accept U+FFFD: an invalid byte reaches the model as
// U+FFFD (the JSON encoder writes it), so it fails closed, matching a
// separator or a letter position (security review 2026-10-21, finding 4).
func isSep(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

func isWild(r rune) bool {
	return r >= utf8.RuneSelf && !unicode.IsSpace(r)
}

// digitLookalike maps the ASCII digits that read as a letter of the name.
func digitLookalike(r rune) rune {
	switch r {
	case '0':
		return 'o'
	case '5':
		return 's'
	case '3':
		return 'e'
	case '7':
		return 't'
	}
	return 0
}

// letterAt returns the ASCII letter pattern state k expects.
func letterAt(k int) rune {
	if k < stMid {
		return rune(markerName[k-1])
	}
	return rune(markerName[k-2])
}

// thread is the set of candidate '<' starts (indices into the candidate
// list) that can reach one pattern state, kept as the hull [lo, hi]. The
// hull may hold starts that cannot really reach the state; escaping one of
// those is harmless, so the over-approximation is safe.
type thread struct{ lo, hi int }

var noThread = thread{-1, -1}

func (t thread) active() bool { return t.lo >= 0 }

func (t thread) union(o thread) thread {
	if !t.active() {
		return o
	}
	if !o.active() {
		return t
	}
	return thread{min(t.lo, o.lo), max(t.hi, o.hi)}
}

// nfa runs the pattern over the model view in one pass (Thompson
// simulation, 19 states per rune), so the scan is linear in the input
// whatever it holds. Security review 2026-10-21: a per-start backtracking
// match took 47 s on 1 MiB of "< ".
type nfa struct {
	cur, next [stMatched + 1]thread
	live      bool
}

func (n *nfa) reset() {
	for i := range n.cur {
		n.cur[i] = noThread
	}
	n.live = false
}

// spawn starts a thread for candidate idx in the lead state.
func (n *nfa) spawn(idx int) {
	n.cur[stLead] = n.cur[stLead].union(thread{idx, idx})
	n.live = true
}

// step feeds one view rune and returns the starts that completed a match.
func (n *nfa) step(r rune) (matched thread) {
	if !n.live {
		return noThread
	}
	// epsilon: a separator run may be empty.
	n.cur[1] = n.cur[1].union(n.cur[stLead])
	n.cur[stMid+1] = n.cur[stMid+1].union(n.cur[stMid])
	for i := range n.next {
		n.next[i] = noThread
	}
	sep, wild := isSep(r), isWild(r)
	live := false
	for k := 0; k < stMatched; k++ {
		t := n.cur[k]
		if !t.active() {
			continue
		}
		if k == stLead || k == stMid {
			if sep {
				n.next[k] = n.next[k].union(t)
				live = true
			}
			continue
		}
		if want := letterAt(k); r == want || wild || digitLookalike(r) == want {
			n.next[k+1] = n.next[k+1].union(t)
			live = true
		}
	}
	matched = n.next[stMatched]
	n.next[stMatched] = noThread
	n.cur, n.live = n.next, live
	return matched
}

// candidate is the byte span of one rune (or escape sequence) that folded
// to '<'.
type candidate struct{ start, end int }

// escapeMarkers rewrites every tag-shaped untrusted_content marker in s so
// a model reads it as text: the rune (or escape sequence) that reads as its
// "<" becomes the literal "&lt;". Everything after it is copied byte for
// byte, so an operator reading a log sees what was written. "&lt;" is
// neither '<' nor an escape this package decodes, so the replacement cannot
// combine with its neighbours into a new marker. When several '<' precede
// one name ("<<untrusted_content"), every one of them is escaped.
func escapeMarkers(s string) string {
	var sc scan
	sc.n.reset()
	for p := 0; p < len(s); {
		// With no live thread, only a possible '<' matters: skip the rest
		// without folding it.
		if !sc.n.live {
			if size, skip := cannotStartMarker(s, p); skip {
				p += size
				continue
			}
		}
		runes, size := sc.f.fold(s, p)
		for _, r := range runes {
			sc.mark(sc.n.step(r))
		}
		if len(runes) == 1 && runes[0] == '<' {
			sc.cands = append(sc.cands, candidate{p, p + size})
			sc.marked = append(sc.marked, false)
			sc.n.spawn(len(sc.cands) - 1)
		}
		p += size
	}
	// A '<' whose thread is still in the lead state at the end of the
	// content would read together with the wrapper's own "\n</untrusted_content>"
	// that follows it; escape it too.
	if sc.n.live {
		sc.mark(sc.n.cur[stLead])
	}
	return sc.render(s)
}

// scan is escapeMarkers' state: the folder, the NFA, every '<' candidate
// seen and which of them to escape.
type scan struct {
	f      folder
	n      nfa
	cands  []candidate
	marked []bool
	hits   bool
}

func (sc *scan) mark(t thread) {
	if !t.active() {
		return
	}
	for i := t.lo; i <= t.hi; i++ {
		sc.marked[i] = true
	}
	sc.hits = true
}

func (sc *scan) render(s string) string {
	if !sc.hits {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	copied := 0
	for i, cd := range sc.cands {
		if !sc.marked[i] {
			continue
		}
		b.WriteString(s[copied:cd.start])
		b.WriteString("&lt;")
		copied = cd.end
	}
	b.WriteString(s[copied:])
	return b.String()
}

// cannotStartMarker reports whether the rune at s[p] can neither read as
// '<' nor begin an escape sequence, and its size.
func cannotStartMarker(s string, p int) (size int, skip bool) {
	if c := s[p]; c < utf8.RuneSelf {
		return 1, c != '<' && !isIntroducer(rune(c))
	}
	r, size := utf8.DecodeRuneInString(s[p:])
	return size, !lessThanRunes[r]
}

// Package readability measures prose deterministically, for the readability
// half of a review (GitHub #633; design
// https://docs.vornik.io §6.2).
//
// It counts what can be counted without a model: words, sentences, sentence
// length, and matches of two published English word lists (hedges and vague
// references). It does not judge jargon, passive voice, buried leads or
// repetition, and it computes no reading grade: those need judgment or are
// wrong outside English. The reviewer judges them.
//
// A word list is a denylist, so a report states its denominators: how many
// words and sentences it examined, how many lines it skipped and why, and how
// many terms each list holds. The lists are English on every run; on text in
// another language they find nothing, which does not mean the text is clean.
package readability

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Finding kinds.
const (
	KindLongSentence = "long-sentence"
	KindHedge        = "hedge"
	KindVague        = "vague-reference"
)

// Reasons a line was not examined.
const (
	SkipCode        = "code"
	SkipTable       = "table"
	SkipURL         = "url"
	SkipFrontMatter = "front-matter"
	SkipComment     = "html-comment"
)

const (
	// longSentenceWords is the length above which a sentence is reported.
	longSentenceWords = 30
	// quoteMaxBytes bounds one quoted passage, marker included.
	quoteMaxBytes = 160
	// maxFindings bounds the findings a rendered report lists.
	maxFindings = 20
	// maxReportBytes bounds a rendered report: a quarter of the companion
	// result's 64 KiB inline budget (design §6.3).
	maxReportBytes = 16 * 1024
)

// hedgeTerms are qualifiers that weaken a claim without stating what is
// uncertain. Matched case-insensitively as whole words.
var hedgeTerms = []string{
	"generally", "usually", "typically", "mostly", "probably", "possibly",
	"perhaps", "arguably", "somewhat", "fairly", "relatively", "basically",
	"essentially", "potentially", "likely", "seems to", "appears to",
	"kind of", "sort of", "more or less", "to some extent", "in some cases",
	"in general", "should mostly", "it is worth noting", "it's worth noting",
	"it should be noted", "needless to say",
}

// vagueTerms are references a reader cannot resolve without searching. A
// sentence that also carries an anchor (a link, a URL, inline code or a
// section sign) is not reported.
var vagueTerms = []string{
	"see above", "see below", "as mentioned", "as discussed", "as noted",
	"as described above", "as stated above", "the above", "the aforementioned",
	"mentioned earlier", "discussed earlier", "previously mentioned",
	"the relevant file", "the relevant code", "the relevant section",
}

var (
	hedgeRE = termsRE(hedgeTerms)
	vagueRE = termsRE(vagueTerms)

	inlineCodeRE = regexp.MustCompile("`[^`]*`")
	linkRE       = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	urlRE        = regexp.MustCompile(`<?https?://[^\s>)]+>?`)
	urlOnlyRE    = regexp.MustCompile(`^<?https?://\S+>?$`)
	headingRE    = regexp.MustCompile(`^#{1,6}\s+`)
	listItemRE   = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	sentenceEnd  = regexp.MustCompile(`[.!?]+(?:\s+|$)`)
)

// anchor stands in for a removed link, URL or code span: not a word, but it
// tells the vague-reference check that the sentence points somewhere.
const anchor = "\x00"

// termsRE matches any term as a whole word, longest first so a phrase wins
// over a shorter term inside it.
func termsRE(terms []string) *regexp.Regexp {
	sorted := append([]string(nil), terms...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	quoted := make([]string, len(sorted))
	for i, t := range sorted {
		quoted[i] = regexp.QuoteMeta(t)
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)\b`)
}

// Finding is one reported passage.
type Finding struct {
	Line  int    `json:"line"`
	Kind  string `json:"kind"`
	Quote string `json:"quote"`
}

// Report is what Measure found in one text.
type Report struct {
	Words             int            `json:"words"`
	Sentences         int            `json:"sentences"`
	MeanSentenceWords float64        `json:"mean_sentence_words"`
	P90SentenceWords  int            `json:"p90_sentence_words"`
	LongSentences     []Finding      `json:"long_sentences"`
	Hedges            []Finding      `json:"hedges"`
	VagueReferences   []Finding      `json:"vague_references"`
	LinesSkipped      map[string]int `json:"lines_skipped"`
	HedgeTerms        int            `json:"hedge_terms"`
	VagueTerms        int            `json:"vague_terms"`
	ListsLanguage     string         `json:"lists_language"`
}

// segment is one prose line of a unit (a paragraph, heading or list item).
type segment struct {
	line int
	text string
}

// Line is one line of text with its line number in the file it came from.
type Line struct {
	N    int
	Text string
}

// Measure examines Markdown prose. It skips front matter, fenced and indented
// code, table rows, lines that are only a URL, and HTML comments, counting
// each skipped line by reason.
func Measure(text string) Report {
	split := strings.Split(text, "\n")
	lines := make([]Line, len(split))
	for i, l := range split {
		lines[i] = Line{N: i + 1, Text: l}
	}
	return MeasureLines(lines)
}

// MeasureLines is Measure over lines that need not be contiguous, such as the
// added lines of a diff. Findings carry each line's own number, and a gap
// between numbers ends a paragraph like a blank line. Its work grows with the
// lines given, never with how large their numbers are.
func MeasureLines(lines []Line) Report {
	r := Report{
		LinesSkipped:  map[string]int{},
		HedgeTerms:    len(hedgeTerms),
		VagueTerms:    len(vagueTerms),
		ListsLanguage: "en",
	}
	var (
		unit    []segment
		lengths []int
		lc      = lineClassifier{prevBlank: true}
	)
	flush := func() {
		lengths = append(lengths, r.measureUnit(unit)...)
		unit = nil
	}
	prevN := 0
	for i, line := range lines {
		ln, raw := line.N, line.Text
		if i > 0 && ln != prevN+1 {
			flush()
			lc.prevBlank, lc.prevCode = true, false
		}
		prevN = ln
		trim := strings.TrimSpace(raw)
		switch reason := lc.classify(ln, raw, trim); reason {
		case "":
		case lineBlank:
			flush()
			continue
		default:
			r.LinesSkipped[reason]++
			flush()
			continue
		}
		if loc := headingRE.FindStringIndex(trim); loc != nil {
			flush()
			unit = []segment{{ln, trim[loc[1]:]}}
			continue
		}
		if loc := listItemRE.FindStringIndex(raw); loc != nil {
			flush()
			unit = []segment{{ln, raw[loc[1]:]}}
			continue
		}
		unit = append(unit, segment{ln, trim})
	}
	flush()
	r.Sentences = len(lengths)
	for _, n := range lengths {
		r.Words += n
	}
	if r.Sentences > 0 {
		r.MeanSentenceWords = float64(r.Words) / float64(r.Sentences)
		sorted := append([]int(nil), lengths...)
		sort.Ints(sorted)
		r.P90SentenceWords = sorted[int(math.Ceil(0.9*float64(len(sorted))))-1]
	}
	return r
}

// lineBlank is classify's answer for an empty line: not prose, not skipped.
const lineBlank = "blank"

// lineClassifier carries the state that decides whether a line is prose:
// inside front matter, a code fence or an HTML comment, and whether the line
// before was blank or indented code.
type lineClassifier struct {
	inFront, inFence, inComment bool
	prevBlank, prevCode         bool
}

// classify returns "" for a prose line, lineBlank for an empty one, or the
// reason a line is skipped. Front matter opens only on line 1.
func (c *lineClassifier) classify(n int, raw, trim string) string {
	switch {
	case n == 1 && trim == "---":
		c.inFront = true
		return SkipFrontMatter
	case c.inFront:
		c.inFront = trim != "---"
		return SkipFrontMatter
	case strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~"):
		c.inFence = !c.inFence
		return SkipCode
	case c.inFence:
		return SkipCode
	case c.inComment:
		c.inComment = !strings.Contains(trim, "-->")
		return SkipComment
	case strings.HasPrefix(trim, "<!--"):
		c.inComment = !strings.Contains(trim[4:], "-->")
		return SkipComment
	case trim == "":
		c.prevBlank = true
		return lineBlank
	case isIndented(raw) && (c.prevBlank || c.prevCode):
		c.prevCode, c.prevBlank = true, false
		return SkipCode
	case strings.HasPrefix(trim, "|"):
		return SkipTable
	case urlOnlyRE.MatchString(trim):
		return SkipURL
	}
	c.prevBlank, c.prevCode = false, false
	return ""
}

// isIndented reports four spaces or a tab of leading indentation.
func isIndented(s string) bool {
	return strings.HasPrefix(s, "    ") || strings.HasPrefix(s, "\t")
}

// measureUnit splits one unit into sentences, records findings, and returns
// each sentence's word count. A sentence keeps the line it starts on.
func (r *Report) measureUnit(unit []segment) []int {
	var (
		lengths []int
		cur     strings.Builder
		start   int
	)
	end := func() {
		raw := strings.TrimSpace(cur.String())
		cur.Reset()
		if raw == "" {
			return
		}
		if n := r.measureSentence(start, raw); n > 0 {
			lengths = append(lengths, n)
		}
	}
	for _, seg := range unit {
		rest := seg.text
		for rest != "" {
			if cur.Len() == 0 {
				start = seg.line
			}
			loc := sentenceEnd.FindStringIndex(rest)
			if loc == nil {
				cur.WriteString(rest)
				cur.WriteString(" ")
				break
			}
			cur.WriteString(rest[:loc[1]])
			rest = rest[loc[1]:]
			end()
		}
	}
	end()
	return lengths
}

// measureSentence records one sentence's findings and returns its word count.
func (r *Report) measureSentence(line int, raw string) int {
	clean := linkRE.ReplaceAllString(raw, "$1 "+anchor)
	clean = inlineCodeRE.ReplaceAllString(clean, anchor)
	clean = urlRE.ReplaceAllString(clean, anchor)
	words := 0
	for _, f := range strings.Fields(clean) {
		if strings.IndexFunc(f, unicode.IsLetter) >= 0 {
			words++
		}
	}
	if words == 0 {
		return 0
	}
	q := quote(raw)
	if words > longSentenceWords {
		r.LongSentences = append(r.LongSentences, Finding{line, KindLongSentence, q})
	}
	for range hedgeRE.FindAllStringIndex(clean, -1) {
		r.Hedges = append(r.Hedges, Finding{line, KindHedge, q})
	}
	if !strings.Contains(clean, anchor) && !strings.Contains(clean, "§") {
		for range vagueRE.FindAllStringIndex(clean, -1) {
			r.VagueReferences = append(r.VagueReferences, Finding{line, KindVague, q})
		}
	}
	return words
}

// quote bounds a passage to quoteMaxBytes, cutting on a rune boundary and
// marking the cut.
func quote(s string) string {
	if len(s) <= quoteMaxBytes {
		return s
	}
	const mark = "…"
	cut := quoteMaxBytes - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

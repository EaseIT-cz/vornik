package readability

// GitHub #633 (https://docs.vornik.io
// §6.2): the deterministic half of the readability review. Every metric is
// pinned on a fixed input, including what the package skips and the
// denominators it publishes.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestMeasure_CountsWordsAndSentences(t *testing.T) {
	r := Measure("The daemon starts. It reads the config and opens the database.\n")
	require.Equal(t, 11, r.Words)
	require.Equal(t, 2, r.Sentences)
	require.InDelta(t, 5.5, r.MeanSentenceWords, 0.01)
	require.Equal(t, 8, r.P90SentenceWords)
}

func TestMeasure_ParagraphLinesJoinIntoOneSentence(t *testing.T) {
	r := Measure("The daemon reads\nthe config file\nat start.\n")
	require.Equal(t, 1, r.Sentences)
	require.Equal(t, 8, r.Words)
}

func TestMeasure_HeadingsAndListItemsAreTheirOwnUnits(t *testing.T) {
	r := Measure("# Title\n\n- first item\n- second item\n1. third item\n")
	require.Equal(t, 4, r.Sentences)
}

func TestMeasure_LongSentenceHasLineAndQuote(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 31)) + "."
	r := Measure("Short one.\n\n" + long + "\n")
	require.Len(t, r.LongSentences, 1)
	require.Equal(t, 3, r.LongSentences[0].Line)
	require.Equal(t, KindLongSentence, r.LongSentences[0].Kind)
	require.True(t, strings.HasPrefix(r.LongSentences[0].Quote, "word word"))
	// Exactly 30 words is not long.
	r = Measure(strings.TrimSpace(strings.Repeat("word ", 30)) + ".\n")
	require.Empty(t, r.LongSentences)
}

func TestMeasure_HedgesFoundWithLine(t *testing.T) {
	r := Measure("Intro.\nThis generally works. It is worth noting that it probably fails.\n")
	require.Len(t, r.Hedges, 3)
	for _, h := range r.Hedges {
		require.Equal(t, 2, h.Line)
		require.Equal(t, KindHedge, h.Kind)
	}
	require.Equal(t, len(hedgeTerms), r.HedgeTerms)
	require.Greater(t, r.HedgeTerms, 20, "the list is published as a denominator")
}

func TestMeasure_HedgeMatchesWholeWordsOnly(t *testing.T) {
	r := Measure("The generalization holds.\n")
	require.Empty(t, r.Hedges)
}

func TestMeasure_VagueReferenceUnlessAnchored(t *testing.T) {
	r := Measure("As mentioned, the relevant file changed. See above.\n")
	require.Len(t, r.VagueReferences, 3)
	require.Equal(t, len(vagueTerms), r.VagueTerms)
	// A sentence that carries an anchor (a link or a section sign) resolves.
	r = Measure("See above, in [the scheduler](#scheduler). As mentioned in §4, it runs.\n")
	require.Empty(t, r.VagueReferences)
}

func TestMeasure_SkipsCodeTablesURLsFrontMatterAndComments(t *testing.T) {
	text := strings.Join([]string{
		"---",
		"title: generally ignored",
		"---",
		"Prose line one.",
		"```go",
		"// this probably panics",
		"x := 1",
		"```",
		"| col | probably |",
		"|-----|----------|",
		"https://example.com/see-above",
		"<!-- generally a comment",
		"still a comment -->",
		"",
		"    indented code probably",
		"",
		"Inline `generally` code and https://x.test/probably are skipped.",
	}, "\n")
	r := Measure(text)
	require.Empty(t, r.Hedges, "nothing skipped may be matched: %+v", r.Hedges)
	require.Equal(t, 3, r.LinesSkipped[SkipFrontMatter])
	require.Equal(t, 5, r.LinesSkipped[SkipCode], "four fenced lines and one indented line")
	require.Equal(t, 2, r.LinesSkipped[SkipTable])
	require.Equal(t, 1, r.LinesSkipped[SkipURL])
	require.Equal(t, 2, r.LinesSkipped[SkipComment])
	require.Equal(t, 2, r.Sentences)
	require.Equal(t, 8, r.Words, `"Prose line one" (3) + "Inline code and are skipped" (5)`)
}

func TestMeasure_ListContinuationIsNotIndentedCode(t *testing.T) {
	r := Measure("- item that\n    continues probably here\n")
	require.Len(t, r.Hedges, 1)
	require.Zero(t, r.LinesSkipped[SkipCode])
}

func TestMeasure_NonEnglishTextCountsWordsAndFindsNothing(t *testing.T) {
	r := Measure("Démon načte konfiguraci a otevře databázi. Pak spustí plánovač úloh.\n")
	require.Equal(t, 10, r.Words)
	require.Equal(t, 2, r.Sentences)
	require.Empty(t, r.Hedges)
	require.Empty(t, r.VagueReferences)
	require.Equal(t, "en", r.ListsLanguage, "set on every run: the lists are English whatever the text is")
}

func TestMeasure_EmptyText(t *testing.T) {
	r := Measure("")
	require.Zero(t, r.Words)
	require.Zero(t, r.Sentences)
	require.Zero(t, r.MeanSentenceWords)
	require.Zero(t, r.P90SentenceWords)
}

func TestQuote_CappedWithVisibleMarker(t *testing.T) {
	long := strings.Repeat("a", 300)
	q := quote(long)
	require.LessOrEqual(t, len(q), quoteMaxBytes)
	require.True(t, strings.HasSuffix(q, "…"))
	require.Equal(t, "short", quote("short"))
	// Never splits a multi-byte rune.
	q = quote(strings.Repeat("č", 200))
	require.True(t, strings.HasSuffix(q, "…"))
	require.LessOrEqual(t, len(q), quoteMaxBytes)
	require.True(t, utf8.ValidString(q))
}

// Security review of c8954a027: diff mode padded the added lines with blanks
// up to their claimed line numbers, so work grew with numbers a pull request
// author chooses. MeasureLines takes the lines with their numbers, and a gap
// between numbers is a paragraph break: work grows with the input only.
func TestMeasureLines_KeepsLineNumbersAndBreaksAtGaps(t *testing.T) {
	r := MeasureLines([]Line{
		{N: 10, Text: "This generally"},
		{N: 11, Text: "works."},
		{N: 900000000, Text: "Probably far away"},
	})
	require.Equal(t, 2, r.Sentences, "the gap ends the first paragraph")
	require.Len(t, r.Hedges, 2)
	require.Equal(t, 10, r.Hedges[0].Line)
	require.Equal(t, 900000000, r.Hedges[1].Line)
}

func TestMeasureLines_MatchesMeasureOnContiguousText(t *testing.T) {
	text := "# Title\n\nThis generally works. See above.\n- item probably\n"
	var lines []Line
	for i, l := range strings.Split(text, "\n") {
		lines = append(lines, Line{N: i + 1, Text: l})
	}
	require.Equal(t, Measure(text), MeasureLines(lines))
}

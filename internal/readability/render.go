package readability

import (
	"fmt"
	"sort"
	"strings"
)

// File is one measured text, named as the review names it.
type File struct {
	Name   string
	Report Report
}

// NotExamined is a file the measurement did not read, with the reason.
type NotExamined struct {
	Name   string
	Reason string
}

// Render writes the report a review returns: a table per file, the files not
// examined, and the first maxFindings findings by file and line. The result is
// at most maxReportBytes, with a visible marker when cut.
func Render(files []File, notExamined []NotExamined) string {
	var b strings.Builder
	b.WriteString("## Readability (measured by the daemon)\n\n")
	fmt.Fprintf(&b, "Files examined: %d. Not examined: %d.\n", len(files), len(notExamined))
	fmt.Fprintf(&b, "The word lists are English (hedge list: %d terms, vague-reference list: %d terms). "+
		"On text in another language they find nothing, which does not mean the text is clean. "+
		"Jargon, passive voice, buried leads and repetition are not measured here.\n",
		len(hedgeTerms), len(vagueTerms))

	if len(files) > 0 {
		b.WriteString("\n| file | words | sentences | mean words/sentence | p90 | long (>30 words) | hedges | vague references | lines skipped |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, f := range files {
			r := f.Report
			fmt.Fprintf(&b, "| %s | %d | %d | %.1f | %d | %d | %d | %d | %s |\n",
				f.Name, r.Words, r.Sentences, r.MeanSentenceWords, r.P90SentenceWords,
				len(r.LongSentences), len(r.Hedges), len(r.VagueReferences), skippedCell(r.LinesSkipped))
		}
	}

	if len(notExamined) > 0 {
		b.WriteString("\nNot examined:\n")
		for _, n := range notExamined {
			fmt.Fprintf(&b, "- %s: %s\n", n.Name, n.Reason)
		}
	}

	type located struct {
		file string
		Finding
	}
	var all []located
	for _, f := range files {
		var fs []Finding
		fs = append(fs, f.Report.LongSentences...)
		fs = append(fs, f.Report.Hedges...)
		fs = append(fs, f.Report.VagueReferences...)
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].Line < fs[j].Line })
		for _, x := range fs {
			all = append(all, located{f.Name, x})
		}
	}
	if len(all) > 0 {
		fmt.Fprintf(&b, "\nFindings (the first %d, by file and line):\n", maxFindings)
		for i, x := range all {
			if i == maxFindings {
				fmt.Fprintf(&b, "\n%d more findings not shown.\n", len(all)-maxFindings)
				break
			}
			fmt.Fprintf(&b, "- `%s:%d` %s: %q\n", x.file, x.Line, x.Kind, x.Quote)
		}
	}
	return capReport(b.String())
}

// skippedCell renders the skipped-line counts, by reason, in a stable order.
func skippedCell(m map[string]int) string {
	var parts []string
	for _, k := range []string{SkipCode, SkipTable, SkipURL, SkipFrontMatter, SkipComment} {
		if m[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, ", ")
}

// capReport cuts a report at the last line that fits, and says so.
func capReport(s string) string {
	if len(s) <= maxReportBytes {
		return s
	}
	const mark = "\n… report truncated at 16 KiB\n"
	cut := strings.LastIndexByte(s[:maxReportBytes-len(mark)], '\n')
	if cut < 0 {
		cut = maxReportBytes - len(mark)
	}
	return s[:cut] + mark
}

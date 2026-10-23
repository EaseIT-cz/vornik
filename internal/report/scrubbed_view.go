package report

import "strings"

// WithheldLine replaces a line the LineScrubber withheld. Every scrubbed view
// of task text writes this one marker (the agent log tools, the held task
// log's scrubbed view, the explain renderer and the post-mortem explainer),
// so a reader can count withheld lines the same way everywhere.
const WithheldLine = "[withheld: secret]"

// ScrubLines is the one scrub of a multi-line text (process-spawn-law design,
// "Amendment 2026-10-21 (#439, #440)", C1): each line goes through scrub; a
// withheld line becomes WithheldLine. It returns the scrubbed text and how
// many lines were withheld.
//
// A nil scrub withholds every non-empty line: a caller that asked for the
// scrubbed view never gets the raw text because the detector could not be
// built.
func ScrubLines(text string, scrub LineScrubber) (string, int) {
	if text == "" {
		return "", 0
	}
	lines := strings.Split(text, "\n")
	withheld := 0
	for i, l := range lines {
		if scrub == nil {
			if l != "" {
				lines[i] = WithheldLine
				withheld++
			}
			continue
		}
		out, w := scrub(l)
		if w {
			out = WithheldLine
			withheld++
		}
		lines[i] = out
	}
	return strings.Join(lines, "\n"), withheld
}

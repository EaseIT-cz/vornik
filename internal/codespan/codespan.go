// Package codespan finds Markdown code regions in text, so a check can tell
// text that carries a token from text that quotes it. Stdlib only: the agent
// loop (a leaf package) and the hallucination rules both import it, one
// implementation for both (GitHub #612; agent tool dispatch design,
// amendment 2026-11-17).
package codespan

import "regexp"

// fenced matches a triple-backtick fenced block, non-greedy across lines.
var fenced = regexp.MustCompile("(?s)```.*?```")

// inline matches an inline-backtick run. Its interior excludes backticks, so
// a stray triple-backtick sequence the fenced pass missed is not re-matched.
var inline = regexp.MustCompile("`[^`]*`")

// Spans returns the byte ranges [start, end) of the fenced blocks and the
// inline code spans of text: a conservative cover of what Strip removes.
// Fenced bytes are masked with spaces before the inline pass, where Strip
// deletes them, so an inline run may pair across a removed fence. A backtick
// pair that opens before a token and closes after it covers that token; an
// unclosed fence covers at most its own backticks, so a token after it stays
// outside.
func Spans(text string) [][2]int {
	var out [][2]int
	masked := []byte(text)
	for _, m := range fenced.FindAllStringIndex(text, -1) {
		out = append(out, [2]int{m[0], m[1]})
		for i := m[0]; i < m[1]; i++ {
			masked[i] = ' '
		}
	}
	for _, m := range inline.FindAllIndex(masked, -1) {
		out = append(out, [2]int{m[0], m[1]})
	}
	return out
}

// Inside reports whether the byte offset at lies in one of spans.
func Inside(spans [][2]int, at int) bool {
	for _, s := range spans {
		if at >= s[0] && at < s[1] {
			return true
		}
	}
	return false
}

// Strip removes fenced blocks, then inline code spans.
func Strip(text string) string {
	return inline.ReplaceAllString(fenced.ReplaceAllString(text, ""), "")
}

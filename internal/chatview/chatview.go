// Package chatview is the one view of a task's stored error and status text
// that a chat reader gives a conversation (process-spawn-law design,
// "Amendment 2026-10-21 (#480)" J1 and "Amendment 2026-10-24 (#497)" L1):
// the text unchanged only when the conversation's audience is one
// admin-linked principal alone, otherwise report.ScrubLines of it. The
// dispatcher's status tools and the non-tool readers (Telegram's notices and
// /status, the e-mail follow-up, the chat completion notice, reminder
// delivery) all decide through it, so the rule exists once.
package chatview

import (
	"context"
	"unicode/utf8"

	"vornik.io/vornik/internal/report"
)

// AudienceViewer is implemented by a channel that can establish that a
// session's audience is one admin-linked principal alone (a Telegram private
// chat whose owner is admin-linked, a Slack DM whose one participant is). A
// channel with no identity authority (e-mail, GitHub) does not implement it.
type AudienceViewer interface {
	OperatorAudience(ctx context.Context, sessionID string) bool
}

// View returns the view of a stored text: unchanged when operator, otherwise
// report.ScrubLines with scrub. A nil scrub withholds every line, so a reader
// whose scrubber was never wired serves nothing raw.
func View(operator bool, scrub report.LineScrubber) func(string) string {
	if operator {
		return func(s string) string { return s }
	}
	return func(s string) string {
		out, _ := report.ScrubLines(s, scrub)
		return out
	}
}

// ForSession is View for a session of ch: the operator view only when ch
// implements AudienceViewer and says so for sessionID. Anything else (a nil
// channel, a channel with no identity authority, a wrapper that hides the
// viewer) reads the scrubbed view.
func ForSession(ctx context.Context, ch any, sessionID string, scrub report.LineScrubber) func(string) string {
	v, ok := ch.(AudienceViewer)
	return View(ok && v != nil && v.OperatorAudience(ctx, sessionID), scrub)
}

// Cut shortens s to at most n bytes, on a rune boundary, and marks the cut
// with "…". It is applied after View, never before (redact before truncate),
// so a secret straddling the cut was seen whole by the scrubber.
func Cut(s string, n int) string {
	if n < 0 || len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

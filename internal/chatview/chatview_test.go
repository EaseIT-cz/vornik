package chatview

// GitHub #497 (process-spawn-law design, "Amendment 2026-10-24 (#497)", L1):
// the one view of a task's stored error and status text that every chat
// reader takes, decided by the conversation's audience and failing closed.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/report"
)

const secret497 = "ghp_Q4wE7rT0yUQ4wE7rT0yUQ4wE7rT0yUQ4wE"

func scrub497(line string) (string, bool) {
	if strings.Contains(line, "ghp_") {
		return "", true
	}
	return line, false
}

type viewer497 bool

func (v viewer497) OperatorAudience(context.Context, string) bool { return bool(v) }

// wrapper497 embeds nothing of the viewer: a resolver that hands back a
// wrapper around a channel must not inherit an operator audience.
type wrapper497 struct{ inner viewer497 }

func TestChatview497_View(t *testing.T) {
	text := "token " + secret497 + "\nexit status 1"
	if got := View(true, scrub497)(text); got != text {
		t.Errorf("operator view changed the text: %q", got)
	}
	got := View(false, scrub497)(text)
	if strings.Contains(got, secret497) || !strings.Contains(got, report.WithheldLine) || !strings.Contains(got, "exit status 1") {
		t.Errorf("scrubbed view: %q", got)
	}
	if got := View(false, nil)(text); strings.Contains(got, "exit status 1") || strings.Contains(got, secret497) {
		t.Errorf("nil scrub must withhold every line: %q", got)
	}
}

func TestChatview497_ForSession(t *testing.T) {
	text := "token " + secret497
	ctx := context.Background()
	if got := ForSession(ctx, viewer497(true), "s", scrub497)(text); got != text {
		t.Errorf("an operator audience was scrubbed: %q", got)
	}
	for name, ch := range map[string]any{
		"not an operator audience": viewer497(false),
		"no viewer":                struct{}{},
		"nil":                      nil,
	} {
		if got := ForSession(ctx, ch, "s", scrub497)(text); strings.Contains(got, secret497) {
			t.Errorf("%s: raw text served: %q", name, got)
		}
	}
}

func TestChatview497_WrapperChannelIsScrubbed(t *testing.T) {
	got := ForSession(context.Background(), wrapper497{inner: true}, "s", scrub497)("token " + secret497)
	if strings.Contains(got, secret497) {
		t.Errorf("a wrapper channel inherited the operator view: %q", got)
	}
}

// Redact before truncate: a secret straddling a byte cut is withheld whole.
func TestChatview497_CutAfterTheView(t *testing.T) {
	text := strings.Repeat("x", 190) + " " + secret497 + "\nexit status 1"
	got := Cut(View(false, scrub497)(text), 200)
	if strings.Contains(got, "ghp_") {
		t.Errorf("a fragment survived the cut: %q", got)
	}
	if got := Cut("abcdef", 3); got != "abc…" {
		t.Errorf("Cut = %q", got)
	}
	if got := Cut("abc", 3); got != "abc" {
		t.Errorf("Cut kept-short = %q", got)
	}
}

package service

// GitHub #497 (process-spawn-law design, "Amendment 2026-10-24 (#497)", L3):
// the chat completion notice carries the task's last error and status text
// into the originating session. It is raw only when the resolved channel
// says the session's audience is one admin-linked principal (a Slack DM of
// an admin); a Slack channel, e-mail, and a channel that cannot answer read
// the scrubbed view, applied before the cut.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher/dispatchertest"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

// audienceCapture497 is a captureChannel that answers the audience question.
type audienceCapture497 struct {
	*captureChannel
	operator bool
}

func (a audienceCapture497) OperatorAudience(context.Context, string) bool { return a.operator }

func TestChatCompletion497_NoticeViewBySession(t *testing.T) {
	scrub, err := report.NewLineScrubber("testhost")
	if err != nil {
		t.Fatal(err)
	}
	resultSecret := "ghp_" + strings.Repeat("R5tY8uI1oP", 4)[:36]
	run := func(t *testing.T, ch conversation.Channel, capture *captureChannel, success bool, message string) string {
		t.Helper()
		n := notifierFor(ch, map[string]*persistence.ChatAuditEntry{
			"turn-1": {ID: "turn-1", ChatID: "slack:T123/D_ADMIN#main", ProjectID: "p"},
		})
		n.taskTextScrub = scrub
		task := turnTask(dispatchertest.TaskID480, "turn-1")
		if !success {
			task.Status = "FAILED"
			le := dispatchertest.ErrText480
			task.LastError = &le
		}
		n.NotifyTaskCompleted(context.Background(), task, success, message)
		sent := capture.snapshot()
		if len(sent) != 1 {
			t.Fatalf("sent = %d", len(sent))
		}
		return sent[0].Text
	}
	t.Run("operator audience reads raw", func(t *testing.T) {
		c := &captureChannel{name: "slack"}
		if got := run(t, audienceCapture497{c, true}, c, false, dispatchertest.ErrText480); !strings.Contains(got, dispatchertest.Token480) {
			t.Errorf("an admin's DM lost the raw error:\n%s", got)
		}
	})
	for name, mk := range map[string]func(*captureChannel) conversation.Channel{
		"not an operator audience": func(c *captureChannel) conversation.Channel { return audienceCapture497{c, false} },
		"no audience authority":    func(c *captureChannel) conversation.Channel { return c },
	} {
		for _, tc := range []struct {
			name    string
			success bool
			msg     string
		}{{"failure", false, dispatchertest.ErrText480}, {"success", true, "the report: " + resultSecret}} {
			t.Run(name+" "+tc.name, func(t *testing.T) {
				c := &captureChannel{name: "slack"}
				got := run(t, mk(c), c, tc.success, tc.msg)
				for _, s := range append(dispatchertest.Secrets480(), resultSecret) {
					if strings.Contains(got, s) {
						t.Errorf("a planted secret reached the session:\n%s", got)
					}
				}
				if !strings.Contains(got, report.WithheldLine) {
					t.Errorf("no withheld marker:\n%s", got)
				}
			})
		}
	}
}

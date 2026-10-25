package email

// GitHub #497 (process-spawn-law design, "Amendment 2026-10-24 (#497)", L3):
// the e-mail follow-up feeds the thread a task's stored error and status
// text. E-mail has no identity authority and the thread's recipients all
// read it, so the text is always the scrubbed view, applied before the cut.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/dispatcher/dispatchertest"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

func TestEmail497_FollowupAlwaysScrubbed(t *testing.T) {
	scrub, err := report.NewLineScrubber("testhost")
	if err != nil {
		t.Fatal(err)
	}
	resultSecret := "ghp_" + strings.Repeat("R5tY8uI1oP", 4)[:36]
	for name, tc := range map[string]struct {
		success bool
		message string
	}{
		"failure": {false, dispatchertest.ErrText480},
		"success": {true, "the report: " + resultSecret},
	} {
		t.Run(name, func(t *testing.T) {
			ch := newTestChannelForFollowup()
			ch.SetTaskTextScrubber(scrub)
			recv := &captureReceiverForFollowup{}
			ch.recv = recv
			ch.RegisterFollowup("session-1", dispatchertest.TaskID480, "p")
			le := dispatchertest.ErrText480
			task := &persistence.Task{ID: dispatchertest.TaskID480, ProjectID: "p", Status: persistence.TaskStatusFailed, LastError: &le}
			ch.NotifyTaskCompleted(context.Background(), task, tc.success, tc.message)
			got := recv.snapshot()
			if len(got) != 1 {
				t.Fatalf("follow-ups = %d", len(got))
			}
			for _, s := range append(dispatchertest.Secrets480(), resultSecret) {
				if strings.Contains(got[0].Text, s) {
					t.Errorf("a planted secret reached the thread:\n%s", got[0].Text)
				}
			}
			if !strings.Contains(got[0].Text, report.WithheldLine) {
				t.Errorf("no withheld marker:\n%s", got[0].Text)
			}
		})
	}
}

// Unwired, every line is withheld: the follow-up still fires.
func TestEmail497_NoScrubberWithholdsEveryLine(t *testing.T) {
	ch := newTestChannelForFollowup()
	recv := &captureReceiverForFollowup{}
	ch.recv = recv
	ch.RegisterFollowup("session-1", "t1", "p")
	le := "plain failure text"
	ch.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed, LastError: &le}, false, "plain status")
	got := recv.snapshot()
	if len(got) != 1 || strings.Contains(got[0].Text, "plain failure text") || strings.Contains(got[0].Text, "plain status") {
		t.Errorf("unwired scrubber served raw text: %+v", got)
	}
}

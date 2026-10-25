package reminders

// GitHub #497 (process-spawn-law design, "Amendment 2026-10-24 (#497)", L3
// and L4): a reminder's failure body carries the executor's error text, so
// it takes the session's view: raw only when the channel says the session's
// audience is one admin-linked principal. The success deliverable is the
// product the reminder was set up to deliver there and is delivered as
// produced.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

var secret497 = "ghp_" + strings.Repeat("Q4wE7rT0yU", 4)[:36]

// audienceChannel497 is a stub channel that answers the audience question.
type audienceChannel497 struct {
	stubChannel
	operator bool
}

func (a *audienceChannel497) OperatorAudience(context.Context, string) bool { return a.operator }

func notify497(t *testing.T, ch conversation.Channel, success bool, message string) string {
	t.Helper()
	scrub, err := report.NewLineScrubber("testhost")
	if err != nil {
		t.Fatal(err)
	}
	repo := newStubRepo()
	repo.claim = &persistence.Reminder{ID: "rem_497", Kind: persistence.ReminderKindTask, Channel: "telegram", ChannelRef: "42", Content: "Nightly build"}
	n := NewCompletionNotifier(repo, &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}, nil, zerolog.Nop(), time.Now,
		WithTaskTextScrubber(scrub))
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_497"}, success, message)
	var sent []string
	switch c := ch.(type) {
	case *audienceChannel497:
		sent = c.sent
	case *stubChannel:
		sent = c.sent
	}
	if len(sent) != 1 {
		t.Fatalf("sent = %d", len(sent))
	}
	return sent[0]
}

func TestReminders497_FailureBodyViewBySession(t *testing.T) {
	msg := "step build failed: Authorization: Bearer " + secret497
	if got := notify497(t, &audienceChannel497{operator: true}, false, msg); !strings.Contains(got, secret497) {
		t.Errorf("an operator audience lost the raw error: %q", got)
	}
	for name, ch := range map[string]conversation.Channel{
		"not an operator audience": &audienceChannel497{operator: false},
		"no audience authority":    &stubChannel{},
	} {
		got := notify497(t, ch, false, msg)
		if strings.Contains(got, secret497) || !strings.Contains(got, report.WithheldLine) {
			t.Errorf("%s: failure body = %q", name, got)
		}
	}
}

func TestReminders497_SuccessDeliverableIsDeliveredRaw(t *testing.T) {
	got := notify497(t, &stubChannel{}, true, "your report: "+secret497)
	if !strings.Contains(got, secret497) {
		t.Errorf("L4: the success deliverable was altered: %q", got)
	}
}

package reminders

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// markErroredFailsRepo is a stubRepo whose MarkErrored always fails.
type markErroredFailsRepo struct{ *stubRepo }

func (markErroredFailsRepo) MarkErrored(context.Context, string, string) error {
	return errors.New("db down")
}

// TestCompletionNotifier_UndeliverableTaskIsCountedAndWarned — GitHub #369,
// outcome-quality LLD S8.11 row 16d. No resolver, or a channel not configured,
// did MarkErrored only, with its own error discarded: no log and no metric.
// Each now Warns and counts vornik_reminders_task_undeliverable_total{reason}.
func TestCompletionNotifier_UndeliverableTaskIsCountedAndWarned(t *testing.T) {
	cases := []struct {
		reason   string
		resolver ChannelResolver
	}{
		{"no_resolver", nil},
		{"channel_not_configured", &stubResolver{channels: map[string]conversation.Channel{}}},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			rem := &persistence.Reminder{
				ID: "rem_u", Kind: persistence.ReminderKindTask, Channel: "webchat",
				ChannelRef: "chat123", Content: "Digest",
			}
			repo := newStubRepo()
			repo.claim = rem
			buf := &bytes.Buffer{}
			n := NewCompletionNotifier(repo, tc.resolver, nil, zerolog.New(buf), time.Now)
			before := testutil.ToFloat64(metricTaskUndeliverable.WithLabelValues(tc.reason))

			n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_u"}, true, "x")

			if got := testutil.ToFloat64(metricTaskUndeliverable.WithLabelValues(tc.reason)); got != before+1 {
				t.Fatalf("task_undeliverable_total{reason=%s} = %v, want %v", tc.reason, got, before+1)
			}
			if !strings.Contains(buf.String(), `"level":"warn"`) {
				t.Fatalf("want a Warn; log: %s", buf.String())
			}
			if _, ok := repo.errored["rem_u"]; !ok {
				t.Fatal("the reminder must still be marked errored")
			}
		})
	}
}

// TestCompletionNotifier_FailedMarkErroredWarns — GitHub #369,
// outcome-quality LLD S8.11 row 16d: a failed MarkErrored was discarded.
func TestCompletionNotifier_FailedMarkErroredWarns(t *testing.T) {
	rem := &persistence.Reminder{
		ID: "rem_m", Kind: persistence.ReminderKindTask, Channel: "webchat",
		ChannelRef: "chat123", Content: "Digest",
	}
	inner := newStubRepo()
	inner.claim = rem
	buf := &bytes.Buffer{}
	n := NewCompletionNotifier(markErroredFailsRepo{inner}, &stubResolver{channels: map[string]conversation.Channel{}}, nil, zerolog.New(buf), time.Now)

	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_m"}, true, "x")

	if !strings.Contains(buf.String(), "mark_errored failed") {
		t.Fatalf("want a mark_errored Warn; log: %s", buf.String())
	}
}

package api

// GitHub #861, scheduled-reminders design §11 R7: reminders_undelivered names
// every reminder that expired with an error and was due in the last seven
// days, and states how many expired reminders it examined.

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestCheckRemindersUndelivered_861(t *testing.T) {
	now := time.Now().UTC()
	var gotFilter persistence.ReminderListFilter
	h := &DoctorHandlers{}
	if c := h.checkRemindersUndelivered(context.Background()); c.Status != "SKIPPED" {
		t.Fatalf("unwired: %+v", c)
	}
	h.SetReminderLister(func(_ context.Context, f persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
		gotFilter = f
		return []*persistence.Reminder{
			{ID: "rem_recent", Channel: "telegram", FireAt: now.Add(-2 * time.Hour), Status: persistence.ReminderStatusExpired, LastError: "telegram timeout"},
			{ID: "rem_old", Channel: "telegram", FireAt: now.Add(-9 * 24 * time.Hour), Status: persistence.ReminderStatusExpired, LastError: "old"},
		}, nil
	})
	c := h.checkRemindersUndelivered(context.Background())
	if gotFilter.Status != persistence.ReminderStatusExpired || gotFilter.FireAfter.IsZero() || gotFilter.PageSize <= 0 {
		t.Fatalf("filter: %+v", gotFilter)
	}
	if c.Status != "WARNING" || len(c.Items) != 1 || !strings.Contains(c.Items[0], "rem_recent") || !strings.Contains(c.Items[0], "telegram timeout") {
		t.Fatalf("check: %+v", c)
	}
	if !strings.Contains(c.Message, "2 expired reminder(s) examined") {
		t.Fatalf("no denominator: %q", c.Message)
	}
	h.SetReminderLister(func(context.Context, persistence.ReminderListFilter) ([]*persistence.Reminder, error) {
		return nil, nil
	})
	if c := h.checkRemindersUndelivered(context.Background()); c.Status != "OK" || !strings.Contains(c.Message, "0 expired reminder(s) examined") {
		t.Fatalf("none: %+v", c)
	}
}

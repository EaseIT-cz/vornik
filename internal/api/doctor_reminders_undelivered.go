package api

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// remindersUndeliveredWindow is how far back reminders_undelivered looks, by
// the time each reminder was due (scheduled-reminders design §11 R7).
const remindersUndeliveredWindow = 7 * 24 * time.Hour

// remindersUndeliveredPage caps the rows the check reads.
const remindersUndeliveredPage = 200

// SetReminderLister wires reminders_undelivered. Nil leaves it SKIPPED.
func (h *DoctorHandlers) SetReminderLister(fn func(ctx context.Context, f persistence.ReminderListFilter) ([]*persistence.Reminder, error)) {
	h.reminderLister = fn
}

// checkRemindersUndelivered warns on each reminder that expired with an error
// and was due in the last seven days (GitHub #861): its delivery failed three
// times, or it was a task-kind reminder whose task could not be created, or
// its cron could not re-arm. The failed channel cannot carry this notice, so
// doctor does. It states how many expired reminders it examined.
func (h *DoctorHandlers) checkRemindersUndelivered(ctx context.Context) DoctorCheck {
	const name = "reminders_undelivered"
	if h.reminderLister == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "reminder store not wired"}
	}
	since := time.Now().Add(-remindersUndeliveredWindow)
	rows, err := h.reminderLister(ctx, persistence.ReminderListFilter{
		Status: persistence.ReminderStatusExpired, FireAfter: since, PageSize: remindersUndeliveredPage})
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read the reminders: " + err.Error()}
	}
	var items []string
	for _, r := range rows {
		if r == nil || r.LastError == "" || r.FireAt.Before(since) {
			continue
		}
		items = append(items, fmt.Sprintf("%s (%s, due %s): %s", r.ID, r.Channel, r.FireAt.UTC().Format(time.RFC3339), r.LastError))
	}
	scope := fmt.Sprintf("%d expired reminder(s) examined, due in the last 7 days", len(rows))
	if len(rows) >= remindersUndeliveredPage {
		scope += fmt.Sprintf(" (the first %d)", remindersUndeliveredPage)
	}
	if len(items) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: scope + "; none undelivered"}
	}
	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: fmt.Sprintf("%s; %d undelivered. Each item gives the last error: a send that failed three times, "+
			"a task that could not be created, a schedule that could not re-arm, or a crash mid-delivery (a task may "+
			"then have been created). Check the channel, then set the reminder again if it is still needed", scope, len(items)),
		Items: items,
	}
}

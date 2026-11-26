package reminders

// GitHub #861, scheduled-reminders design §11: a failed text send is tried
// three times (at its time, +1 min, +5 min), then a one-shot reminder expires
// and a recurring one re-arms; a task-kind fire is not retried; a corrupt cron
// expires. Before the fix every failure left the row 'firing' forever
// (MarkErrored), which these tests would see as no outcome at all.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

var now861 = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

func failingRunner(repo *stubRepo) *Runner {
	ch := &stubChannel{err: errors.New("telegram timeout")}
	return New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		Logger:   zerolog.Nop(),
		Clock:    func() time.Time { return now861 },
	})
}

func TestRunner_861_AFailedSendRetriesThenExpires(t *testing.T) {
	for _, tc := range []struct {
		errorCount int
		want       persistence.ReminderAttemptOutcome
	}{
		{0, persistence.ReminderAttemptOutcome{Kind: persistence.ReminderAttemptRetry, At: now861.Add(time.Minute)}},
		{1, persistence.ReminderAttemptOutcome{Kind: persistence.ReminderAttemptRetry, At: now861.Add(5 * time.Minute)}},
		{2, persistence.ReminderAttemptOutcome{Kind: persistence.ReminderAttemptExpire}},
	} {
		repo := newStubRepo()
		repo.queue = []*persistence.Reminder{{ID: "rem_861", Channel: "telegram", ErrorCount: tc.errorCount}}
		failingRunner(repo).tickOnce(context.Background())
		if got := repo.attempts["rem_861"]; got != tc.want {
			t.Errorf("after %d earlier failures: %+v, want %+v", tc.errorCount, got, tc.want)
		}
		if repo.errored["rem_861"] == "" {
			t.Errorf("after %d earlier failures: the error was not kept", tc.errorCount)
		}
	}
}

func TestRunner_861_ARecurringReminderReArmsAfterTheThirdFailure(t *testing.T) {
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{{ID: "rem_rec", Channel: "telegram", CronExpr: "0 7 * * *", ErrorCount: 2}}
	failingRunner(repo).tickOnce(context.Background())
	got := repo.attempts["rem_rec"]
	next, _ := NextFireAt("0 7 * * *", now861)
	if got.Kind != persistence.ReminderAttemptRearm || !got.At.Equal(next) {
		t.Fatalf("got %+v, want rearm at %v", got, next)
	}
}

func TestRunner_861_ATaskKindFailureIsNotRetried(t *testing.T) {
	for _, recurring := range []bool{false, true} {
		repo := newStubRepo()
		rem := &persistence.Reminder{ID: "rem_task", Kind: persistence.ReminderKindTask, ProjectID: "p", Channel: "telegram", Content: "digest"}
		if recurring {
			rem.CronExpr = "0 7 * * *"
		}
		repo.queue = []*persistence.Reminder{rem}
		creator := &stubCreator{err: errors.New("queue full")}
		r := New(Config{Repo: repo, Logger: zerolog.Nop(), Clock: func() time.Time { return now861 }})
		r.SetCreator(creator)
		r.tickOnce(context.Background())
		want := persistence.ReminderAttemptExpire
		if recurring {
			want = persistence.ReminderAttemptRearm
		}
		if got := repo.attempts["rem_task"]; got.Kind != want {
			t.Errorf("recurring=%v: %+v, want %s", recurring, got, want)
		}
		if creator.calls != 1 {
			t.Errorf("recurring=%v: the creator was called %d times, want 1", recurring, creator.calls)
		}
	}
}

func TestRunner_861_ACorruptCronExpires(t *testing.T) {
	repo := newStubRepo()
	repo.queue = []*persistence.Reminder{{ID: "rem_bad", Channel: "telegram", CronExpr: "not a cron"}}
	ch := &stubChannel{}
	r := New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		Logger:   zerolog.Nop(),
		Clock:    func() time.Time { return now861 },
	})
	r.tickOnce(context.Background())
	if got := repo.attempts["rem_bad"]; got.Kind != persistence.ReminderAttemptExpire {
		t.Fatalf("a corrupt cron after a send: %+v, want expire", got)
	}
}

func TestRunner_861_TheSweepCountsACrashedRowAsAnAttempt(t *testing.T) {
	repo := newStubRepo()
	repo.stuckQueue = []*persistence.Reminder{
		{ID: "stuck_text", Channel: "telegram", Status: persistence.ReminderStatusFiring},
		{ID: "stuck_task", Kind: persistence.ReminderKindTask, Status: persistence.ReminderStatusFiring},
	}
	r := failingRunner(repo)
	r.sweepStuckFiring(context.Background())
	if got := repo.attempts["stuck_text"]; got.Kind != persistence.ReminderAttemptRetry || !got.At.Equal(now861.Add(time.Minute)) {
		t.Errorf("a crashed one-shot text row: %+v, want a retry in a minute", got)
	}
	if got := repo.attempts["stuck_task"]; got.Kind != persistence.ReminderAttemptExpire {
		t.Errorf("a crashed one-shot task-kind row: %+v, want expire", got)
	}
}

// stubCreator counts its calls, so a test can assert no retry re-created a task.
type stubCreator struct {
	err   error
	calls int
}

func (c *stubCreator) CreateScheduledTask(context.Context, ScheduledTaskParams) (string, error) {
	c.calls++
	return "", c.err
}

// cancelledRepo is a stubRepo whose reminder was cancelled between lease and
// failure: the guarded write finds no firing row.
type cancelledRepo struct {
	*stubRepo
	writes int
}

func (c *cancelledRepo) MarkAttemptFailed(context.Context, string, string, persistence.ReminderAttemptOutcome) error {
	c.writes++
	return persistence.ErrNotFound
}

// A reminder cancelled between lease and failure stays cancelled: the runner
// makes the one guarded write, takes its ErrNotFound, and writes nothing else.
func TestRunner_861_ACancelBetweenLeaseAndFailureStaysCancelled(t *testing.T) {
	base := newStubRepo()
	repo := &cancelledRepo{stubRepo: base}
	base.queue = []*persistence.Reminder{{ID: "rem_cancelled", Channel: "telegram"}}
	ch := &stubChannel{err: errors.New("telegram timeout")}
	r := New(Config{
		Repo:     repo,
		Resolver: &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}},
		Logger:   zerolog.Nop(),
		Clock:    func() time.Time { return now861 },
	})
	r.tickOnce(context.Background())
	if repo.writes != 1 || len(base.fired) != 0 || len(base.rescheduled) != 0 || len(base.errored) != 0 {
		t.Fatalf("writes %d, fired %v, rescheduled %v, errored %v", repo.writes, base.fired, base.rescheduled, base.errored)
	}
}

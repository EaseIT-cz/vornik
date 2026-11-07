package telegram

// GitHub #644 (dispatcher turn-context design §8). Found at: production,
// 2026-11-04: a looping turn could only be watched until it hit the
// iteration cap, twice.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// stopAwareReceiver blocks each turn until its context ends, recording the
// cancel cause and the order of turns.
type stopAwareReceiver struct {
	started chan string
	mu      sync.Mutex
	causes  []error
	texts   []string
}

func (r *stopAwareReceiver) Receive(ctx context.Context, msg conversation.ChannelMessage) error {
	r.started <- msg.Text
	<-ctx.Done()
	r.mu.Lock()
	r.causes = append(r.causes, context.Cause(ctx))
	r.texts = append(r.texts, msg.Text)
	r.mu.Unlock()
	return ctx.Err()
}

func waitStarted(t *testing.T, r *stopAwareReceiver) string {
	t.Helper()
	select {
	case s := <-r.started:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	return ""
}

func TestStop_EndsRunningTurn_AndQueuedMessageStillRuns_644(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	r := &stopAwareReceiver{started: make(chan string, 4)}
	bot.SetReceiver(r)
	ctx := context.Background()

	_ = bot.HandleMessage(ctx, &Message{ID: 1, ChatID: 100, UserID: 42, Text: "loop forever"})
	if got := waitStarted(t, r); got != "loop forever" {
		t.Fatalf("first turn = %q", got)
	}
	// A second message queues behind the running turn.
	_ = bot.HandleMessage(ctx, &Message{ID: 2, ChatID: 100, UserID: 42, Text: "next question"})

	reply := bot.handleStopCmd(ctx, 100, 42, []string{"/stop"})
	if !strings.HasPrefix(reply, "Stopped.") {
		t.Fatalf("reply = %q", reply)
	}
	// The queued message runs after the stopped turn (§8.2, review d298 F2).
	if got := waitStarted(t, r); got != "next question" {
		t.Fatalf("queued turn = %q", got)
	}
	r.mu.Lock()
	cause := r.causes[0]
	r.mu.Unlock()
	if cause != dispatcher.ErrStoppedByUser {
		t.Fatalf("cause = %v, want ErrStoppedByUser", cause)
	}
	// The earlier /stop did not touch the later turn (review 6f20 F3).
	bot.stops.mu.Lock()
	_, stillRunning := bot.stops.running[100]
	bot.stops.mu.Unlock()
	if !stillRunning {
		t.Fatal("the queued turn must be running, untouched by the earlier /stop")
	}
	_ = bot.handleStopCmd(ctx, 100, 42, []string{"/stop"}) // end the second turn
	waitCauses(t, r, 2)
	r.mu.Lock()
	second := r.causes[1]
	r.mu.Unlock()
	if second != dispatcher.ErrStoppedByUser {
		t.Fatalf("second turn cause = %v; it ended only on its own /stop", second)
	}
}

func waitCauses(t *testing.T, r *stopAwareReceiver, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.causes)
		r.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d finished turns", n)
}

func TestStop_NothingRunning_644(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	if got := bot.handleStopCmd(context.Background(), 100, 42, []string{"/stop"}); got != "Nothing is running in this chat." {
		t.Fatalf("reply = %q", got)
	}
}

// A finished turn's cleanup must not remove a newer turn's entry (review d298
// F3).
func TestStop_StaleCleanupKeepsNewerTurn_644(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	_, doneOld := bot.beginTurn(100, false, 42)
	ctxNew, doneNew := bot.beginTurn(100, false, 42)
	defer doneNew()
	doneOld()
	if got := bot.handleStopCmd(context.Background(), 100, 42, []string{"/stop"}); !strings.HasPrefix(got, "Stopped.") {
		t.Fatalf("the newer turn must still be stoppable: %q", got)
	}
	if context.Cause(ctxNew) != dispatcher.ErrStoppedByUser {
		t.Fatal("the newer turn was not the one stopped")
	}
}

func TestStop_ListsAndCancelsTheTurnsTasks_644(t *testing.T) {
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()
	now := time.Now().UTC()
	wf := "wf"
	for _, id := range []string{"task-new", "task-old", "task-other-chat"} {
		if err := tasks.Create(ctx, &persistence.Task{ID: id, ProjectID: "p", WorkflowID: &wf, Status: persistence.TaskStatusRunning,
			Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	bot := makeBotWithTaskRepo(t, tasks)
	WithTaskTeardown(&tgTeardownSpy{})(bot)
	clock := now
	bot.stops.now = func() time.Time { return clock }

	bot.RegisterFollowup(100, "task-old", "p") // before the turn
	bot.followupMu.Lock()
	fu := bot.pendingFollowups["task-old"]
	fu.createdAt = now.Add(-time.Minute)
	bot.pendingFollowups["task-old"] = fu
	bot.followupMu.Unlock()

	_, done := bot.beginTurn(100, false, 42)
	defer done()
	bot.RegisterFollowup(100, "task-new", "p")
	bot.RegisterFollowup(200, "task-other-chat", "p")

	reply := bot.handleStopCmd(ctx, 100, 0, []string{"/stop"})
	if !strings.Contains(reply, "task-new") || strings.Contains(reply, "task-old") || strings.Contains(reply, "task-other-chat") {
		t.Fatalf("the reply must list only this turn's tasks: %q", reply)
	}
	got, _ := tasks.Get(ctx, "task-new")
	if got.Status != persistence.TaskStatusRunning {
		t.Fatal("/stop alone must not cancel tasks")
	}
	out := bot.handleStopCmd(ctx, 100, 0, []string{"/stop", "tasks"})
	got, _ = tasks.Get(ctx, "task-new")
	if got.Status != persistence.TaskStatusCancelled || !strings.Contains(out, "Cancelled") {
		t.Fatalf("/stop tasks must cancel the listed task: status %s, reply %q", got.Status, out)
	}
	if again := bot.handleStopCmd(ctx, 100, 0, []string{"/stop", "tasks"}); !strings.HasPrefix(again, "No stopped turn") {
		t.Fatalf("a second /stop tasks has nothing to do: %q", again)
	}
}

func TestStopTasks_WindowExpires_644(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	clock := time.Now()
	bot.stops.now = func() time.Time { return clock }
	_, done := bot.beginTurn(100, false, 42)
	bot.RegisterFollowup(100, "task-x", "p")
	_ = bot.handleStopCmd(context.Background(), 100, 0, []string{"/stop"})
	done()
	clock = clock.Add(stopTasksWindow + time.Second)
	if got := bot.handleStopCmd(context.Background(), 100, 0, []string{"/stop", "tasks"}); !strings.HasPrefix(got, "No stopped turn") {
		t.Fatalf("after the window: %q", got)
	}
}

// A refused cancel is reported per task (review 81b7 F2), an idle /stop keeps
// the pending list (F6), and /help names /stop (F3).
func TestStopTasks_RefusalReported_IdleStopKeepsList_644(t *testing.T) {
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()
	now := time.Now().UTC()
	wf := "wf"
	if err := tasks.Create(ctx, &persistence.Task{ID: "task-done", ProjectID: "p", WorkflowID: &wf, Status: persistence.TaskStatusRunning,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	bot := makeBotWithTaskRepo(t, tasks)
	WithTaskTeardown(&tgTeardownSpy{})(bot)
	_, done := bot.beginTurn(100, false, 42)
	bot.RegisterFollowup(100, "task-done", "p")
	_ = bot.handleStopCmd(ctx, 100, 0, []string{"/stop"})
	done()
	// An idle /stop must not clear the pending list.
	if got := bot.handleStopCmd(ctx, 100, 0, []string{"/stop"}); got != "Nothing is running in this chat." {
		t.Fatalf("idle /stop: %q", got)
	}
	// The task finishes before the user confirms: the cancel is refused and
	// reported.
	got, _ := tasks.Get(ctx, "task-done")
	got.Status = persistence.TaskStatusCompleted
	if err := tasks.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	out := bot.handleStopCmd(ctx, 100, 0, []string{"/stop", "tasks"})
	if !strings.Contains(out, "task-done was not cancelled") {
		t.Fatalf("a refused cancel must be reported: %q", out)
	}
	if help := handleHelp(ctx, bot, 100, 0); !strings.Contains(help, "/stop") {
		t.Fatal("/help must name /stop")
	}
}

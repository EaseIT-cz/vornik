package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CompanionPushHarness wires the outbox suite to one driver.
type CompanionPushHarness struct {
	Outbox  persistence.CompanionPushOutbox
	Configs persistence.A2APushConfigRepository
	Actions persistence.BrokerActionRepository
	// SeedTask inserts a tasks row; SetStatus changes its status.
	SeedTask  func(t *testing.T, id, projectID, source, status string)
	SetStatus func(t *testing.T, id, status string)
}

// RunCompanionPushSuite is the §7a outbox contract on both drivers.
func RunCompanionPushSuite(t *testing.T, h CompanionPushHarness) {
	t.Run("Terminal_companion_task_is_due_once", func(t *testing.T) { pushTaskDueOnce(t, h) })
	t.Run("A2A_and_running_tasks_are_not_due", func(t *testing.T) { pushTaskNotDue(t, h) })
	t.Run("Mark_is_a_compare_and_set", func(t *testing.T) { pushMarkIsCAS(t, h) })
	t.Run("Actions_are_due_under_front_names", func(t *testing.T) { pushActionsFrontNames(t, h) })
	t.Run("Action_without_config_is_due_with_no_url", func(t *testing.T) { pushActionWithoutConfig(t, h) })
}

func pushDueTask(t *testing.T, h CompanionPushHarness, taskID string) *persistence.TaskPushDue {
	t.Helper()
	due, err := h.Outbox.DueTaskPushes(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	for i := range due {
		if due[i].TaskID == taskID {
			return &due[i]
		}
	}
	return nil
}

func pushTaskDueOnce(t *testing.T, h CompanionPushHarness) {
	ctx := context.Background()
	id := uniqueID("ptask")
	h.SeedTask(t, id, "push-p", "COMPANION", "RUNNING")
	if err := h.Configs.Set(ctx, persistence.A2APushConfig{TaskID: id, URL: "https://hooks.example/x", Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	if pushDueTask(t, h, id) != nil {
		t.Fatal("a running task is not due")
	}
	h.SetStatus(t, id, "COMPLETED")
	d := pushDueTask(t, h, id)
	if d == nil || d.State != "COMPLETED" || d.URL != "https://hooks.example/x" || d.Token != "tok" || d.ProjectID != "push-p" {
		t.Fatalf("due = %+v", d)
	}
	ok, err := h.Outbox.MarkTaskPushed(ctx, id, d.PushedState, d.State)
	if err != nil || !ok {
		t.Fatalf("mark: %v %v", ok, err)
	}
	if pushDueTask(t, h, id) != nil {
		t.Fatal("still due after it was marked")
	}
}

func pushTaskNotDue(t *testing.T, h CompanionPushHarness) {
	ctx := context.Background()
	a2a := uniqueID("pa2a")
	h.SeedTask(t, a2a, "push-p", "A2A", "COMPLETED")
	_ = h.Configs.Set(ctx, persistence.A2APushConfig{TaskID: a2a, URL: "https://hooks.example/a"})
	noCfg := uniqueID("pnocfg")
	h.SeedTask(t, noCfg, "push-p", "COMPANION", "FAILED")
	if pushDueTask(t, h, a2a) != nil {
		t.Fatal("an A2A task is served by the A2A pusher, not the outbox")
	}
	if pushDueTask(t, h, noCfg) != nil {
		t.Fatal("a task without a push config is not due")
	}
}

func pushMarkIsCAS(t *testing.T, h CompanionPushHarness) {
	ctx := context.Background()
	id := uniqueID("pcas")
	h.SeedTask(t, id, "push-p", "COMPANION", "CANCELLED")
	_ = h.Configs.Set(ctx, persistence.A2APushConfig{TaskID: id, URL: "https://hooks.example/c"})
	d := pushDueTask(t, h, id)
	if d == nil {
		t.Fatal("not due")
	}
	if ok, _ := h.Outbox.MarkTaskPushed(ctx, id, d.PushedState, "CANCELLED"); !ok {
		t.Fatal("first mark lost")
	}
	if ok, _ := h.Outbox.MarkTaskPushed(ctx, id, d.PushedState, "CANCELLED"); ok {
		t.Fatal("a second mark with a stale prev must not win")
	}
}

func pushStageAction(t *testing.T, h CompanionPushHarness, taskID, kind string) *persistence.BrokerAction {
	t.Helper()
	now := time.Now().UTC()
	a := &persistence.BrokerAction{
		ActionID: uniqueID("pact"), ProjectID: "push-p", TaskID: taskID, ActionKind: kind, Tool: "mcp__w__send",
		ArgsJSON: []byte(`{}`), ArgsSHA256: "h", Status: persistence.BrokerActionStaged,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if _, err := h.Actions.Stage(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func pushDueAction(t *testing.T, h CompanionPushHarness, actionID string) *persistence.ActionPushDue {
	t.Helper()
	due, err := h.Outbox.DueActionPushes(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	for i := range due {
		if due[i].ActionID == actionID {
			return &due[i]
		}
	}
	return nil
}

func pushActionsFrontNames(t *testing.T, h CompanionPushHarness) {
	ctx := context.Background()
	task := uniqueID("pacttask")
	h.SeedTask(t, task, "push-p", "COMPANION", "COMPLETED")
	_ = h.Configs.Set(ctx, persistence.A2APushConfig{TaskID: task, URL: "https://hooks.example/act", Token: "t2"})
	a := pushStageAction(t, h, task, "reply")
	if pushDueAction(t, h, a.ActionID) != nil {
		t.Fatal("staged is not pushable")
	}
	if _, err := h.Actions.PromoteStaged(ctx, task); err != nil {
		t.Fatal(err)
	}
	d := pushDueAction(t, h, a.ActionID)
	if d == nil || d.State != "pending_approval" || d.Action != "reply" || d.TaskID != task || d.URL == "" || d.Token != "t2" {
		t.Fatalf("pending due = %+v", d)
	}
	if ok, _ := h.Outbox.MarkActionPushed(ctx, a.ActionID, d.PushedState, d.State); !ok {
		t.Fatal("mark lost")
	}
	if pushDueAction(t, h, a.ActionID) != nil {
		t.Fatal("pending_approval pushed twice")
	}
	if err := h.Actions.Approve(ctx, a.ActionID, "h", "op", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if d := pushDueAction(t, h, a.ActionID); d == nil || d.State != "approved" {
		t.Fatalf("approved due = %+v", d)
	}
	// executing is not pushable: the claim leaves pushed_state behind until
	// the terminal write.
	if ok, _ := h.Actions.ClaimForExecution(ctx, a.ActionID, time.Now().UTC()); !ok {
		t.Fatal("claim")
	}
	if d := pushDueAction(t, h, a.ActionID); d != nil {
		t.Fatalf("executing is not pushable, got %+v", d)
	}
	if err := h.Actions.Finish(ctx, a.ActionID, persistence.BrokerActionUnknown, persistence.BrokerOutcomeTimeout, []byte(`{}`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if d := pushDueAction(t, h, a.ActionID); d == nil || d.State != "unknown" {
		t.Fatalf("unknown due = %+v", d)
	}
	// A resolve written from another process is due on the next pass.
	if err := h.Actions.Resolve(ctx, a.ActionID, persistence.BrokerActionExecuted, "op", []byte(`{}`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if d := pushDueAction(t, h, a.ActionID); d == nil || d.State != "executed" {
		t.Fatalf("resolved due = %+v", d)
	}
}

func pushActionWithoutConfig(t *testing.T, h CompanionPushHarness) {
	ctx := context.Background()
	task := uniqueID("pnocfgtask")
	h.SeedTask(t, task, "push-p", "COMPANION", "COMPLETED")
	a := pushStageAction(t, h, task, "reply")
	_, _ = h.Actions.PromoteStaged(ctx, task)
	d := pushDueAction(t, h, a.ActionID)
	if d == nil || d.URL != "" {
		t.Fatalf("an action without a config is due with no URL, so it can be marked: %+v", d)
	}
	a2aTask := uniqueID("pa2aact")
	h.SeedTask(t, a2aTask, "push-p", "A2A", "COMPLETED")
	b := pushStageAction(t, h, a2aTask, "reply")
	_, _ = h.Actions.PromoteStaged(ctx, a2aTask)
	if pushDueAction(t, h, b.ActionID) != nil {
		t.Fatal("an A2A task's actions are never pushed by the outbox")
	}
}

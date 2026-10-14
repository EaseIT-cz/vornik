package service

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/forgeci"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/reminders"
	"vornik.io/vornik/internal/taskcreate"
)

// GitHub #347 (learning-loop design, amendment 2026-10-13): each creation
// site records the surface that wrote the goal. These are the sites the
// service package owns; each asserts the exact constant from the design's
// table, so a third-party site stamped first-party fails.

func TestGitHubTaskCreator_StampsGitHubGoalOrigin(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	if err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind: "issues.labeled", SessionID: "acme/api#issues/5", Title: "bug", Body: "details",
		Labels: []string{"vornik-task"}, SenderLogin: "outsider", Repo: "acme/api", Number: 5,
		IdempotencyKey: "github-app:d-347",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 || tasks[0].GoalOrigin != persistence.TaskGoalOriginGitHub {
		t.Fatalf("want one task with goal_origin %q, got %+v", persistence.TaskGoalOriginGitHub, tasks)
	}
}

func TestGitHubCITask_StampsGitHubCIGoalOrigin(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, projectForTaskCreator("p-1"), nil, zerolog.Nop())
	if err := g.createCITask(context.Background(), github.TaskCreationEvent{Repo: "acme/api", Number: 7, CI: &github.CIRunRef{RunID: 1}},
		&persistence.ForgeCIOutcome{RunID: 1, Conclusion: "failure"}, forgeci.Decision{Enqueue: true}); err != nil {
		t.Fatalf("createCITask: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 || tasks[0].GoalOrigin != persistence.TaskGoalOriginGitHubCI {
		t.Fatalf("want one task with goal_origin %q, got %+v", persistence.TaskGoalOriginGitHubCI, tasks)
	}
}

func TestA2AAdapter_StampsA2AGoalOrigin(t *testing.T) {
	repo := newRecordingTaskRepo()
	adapter := a2aTaskCreatorAdapter{inner: taskcreate.New(taskcreate.WithTaskRepository(repo))}
	if _, err := adapter.Create(context.Background(), a2a.TaskCreateParams{
		ProjectID: "p-1", TaskType: "research", Prompt: "from a peer",
		CreationSource: persistence.TaskCreationSourceA2A,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 || tasks[0].GoalOrigin != persistence.TaskGoalOriginA2A {
		t.Fatalf("want one task with goal_origin %q, got %+v", persistence.TaskGoalOriginA2A, tasks)
	}
}

func TestReminderTaskCreator_StampsReminderGoalOrigin(t *testing.T) {
	inner := &fakeInnerCreator{}
	adapter := &containerTaskCreator{creator: inner}
	if _, err := adapter.CreateScheduledTask(context.Background(), reminders.ScheduledTaskParams{
		ProjectID: "news", Prompt: "digest", TaskType: "research", ReminderID: "rem_1",
	}); err != nil {
		t.Fatalf("CreateScheduledTask: %v", err)
	}
	if inner.got.GoalOrigin != persistence.TaskGoalOriginReminder {
		t.Fatalf("GoalOrigin = %q, want %q", inner.got.GoalOrigin, persistence.TaskGoalOriginReminder)
	}
}

func TestSmokeRunner_StampsDoctorGoalOrigin(t *testing.T) {
	repo := &countingTaskRepo{}
	sr := newSmokeRunner(taskcreate.New(taskcreate.WithTaskRepository(repo)), repo, nil)
	id, err := sr.Trigger(context.Background(), "p1", "prompt")
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if got := repo.byID[id]; got == nil || got.GoalOrigin != persistence.TaskGoalOriginDoctor {
		t.Fatalf("want goal_origin %q, got %+v", persistence.TaskGoalOriginDoctor, got)
	}
}

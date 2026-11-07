package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskcreate"
)

// #617 (federation design, amendment 2026-11-06): A2A tasks named no key.
// Measured: 0 of 73 carried created_by_api_key_id, so no per-key budget or
// quota could see them. The adapter now stamps the request's key.
func TestA2AAdapter_AttributesTheTaskToTheCallerKey_617(t *testing.T) {
	repo := newRecordingTaskRepo()
	adapter := a2aTaskCreatorAdapter{inner: taskcreate.New(taskcreate.WithTaskRepository(repo))}
	ctx := api.ContextWithAPIKeyIDForTesting(context.Background(), "key-caller")
	if _, err := adapter.Create(ctx, a2a.TaskCreateParams{
		ProjectID: "p-1", WorkflowID: "qa", TaskType: "qa", Prompt: "a question",
		CreationSource: persistence.TaskCreationSourceA2A,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 || tasks[0].CreatedByAPIKeyID == nil || *tasks[0].CreatedByAPIKeyID != "key-caller" {
		t.Fatalf("want one task attributed to key-caller, got %+v", tasks)
	}
}

// Without a key (auth off) the task names none, as before.
func TestA2AAdapter_NoKeyNoAttribution_617(t *testing.T) {
	repo := newRecordingTaskRepo()
	adapter := a2aTaskCreatorAdapter{inner: taskcreate.New(taskcreate.WithTaskRepository(repo))}
	if _, err := adapter.Create(context.Background(), a2a.TaskCreateParams{
		ProjectID: "p-1", TaskType: "qa", Prompt: "a question", CreationSource: persistence.TaskCreationSourceA2A,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tasks := repo.snapshotTasks(); len(tasks) != 1 || tasks[0].CreatedByAPIKeyID != nil {
		t.Fatalf("want one unattributed task, got %+v", tasks)
	}
}

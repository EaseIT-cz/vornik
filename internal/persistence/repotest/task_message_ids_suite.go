package repotest

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunTaskMessageIDsSuite is the contract for TaskMessageFilter.IDs (GitHub
// #670, task-lifecycle-conversational design §18a): List returns exactly the
// named rows of the task, never another task's, whatever their position in
// the thread. taskID must be unique per run; the suite creates two tasks.
func RunTaskMessageIDsSuite(t *testing.T, tasks persistence.TaskRepository, msgs persistence.TaskMessageRepository, taskID string) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	wf := "wf"
	other := taskID + "-other"
	for _, id := range []string{taskID, other} {
		if err := tasks.Create(ctx, &persistence.Task{
			ID: id, ProjectID: "p670", WorkflowID: &wf, Status: persistence.TaskStatusRunning,
			CreationSource: persistence.TaskCreationSourceUser, Priority: 50, Payload: []byte(`{}`),
			CreatedAt: base, UpdatedAt: base, MaxAttempts: 1, Attempt: 1,
		}); err != nil {
			t.Fatalf("create task %s: %v", id, err)
		}
	}
	insert := func(task string, i int) string {
		t.Helper()
		id := fmt.Sprintf("%s-m%03d", task, i)
		if err := msgs.Insert(ctx, &persistence.TaskMessage{
			ID: id, TaskID: task, AuthorKind: persistence.TaskMessageAuthorLead,
			MessageKind: persistence.TaskMessageKindMessage, Content: fmt.Sprintf("message %d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		return id
	}
	for i := 0; i < 8; i++ {
		insert(taskID, i)
	}
	foreign := insert(other, 0)
	want := []string{taskID + "-m001", taskID + "-m006"}
	got, err := msgs.List(ctx, persistence.TaskMessageFilter{TaskID: taskID, IDs: append(append([]string{}, want...), foreign, taskID+"-missing")})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("IDs filter returned %v, want %v (only the task's own named rows)", ids, want)
	}
}

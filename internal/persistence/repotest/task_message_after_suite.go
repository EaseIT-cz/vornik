package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunTaskMessageAfterSuite is the contract for TaskMessageFilter.After
// (GitHub #679, task-lifecycle-conversational design §18b): paging continues
// after the cursor row's (created_at, id), the order List returns, so a
// message written in the same second as a page's last row is not skipped when
// its id sorts lower. taskID must be unique per run; the suite creates the task.
func RunTaskMessageAfterSuite(t *testing.T, tasks persistence.TaskRepository, msgs persistence.TaskMessageRepository, taskID string) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	wf := "wf"
	if err := tasks.Create(ctx, &persistence.Task{
		ID: taskID, ProjectID: "p679", WorkflowID: &wf, Status: persistence.TaskStatusRunning,
		CreationSource: persistence.TaskCreationSourceUser, Priority: 50, Payload: []byte(`{}`),
		CreatedAt: base, UpdatedAt: base, MaxAttempts: 1, Attempt: 1,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Same second; the LATER message has the LOWER id, as GenerateID's random
	// suffix allows.
	first := taskID + "-z"
	second := taskID + "-a"
	for i, id := range []string{first, second} {
		if err := msgs.Insert(ctx, &persistence.TaskMessage{
			ID: id, TaskID: taskID, AuthorKind: persistence.TaskMessageAuthorOperator,
			MessageKind: persistence.TaskMessageKindMessage, Content: id,
			CreatedAt: base.Add(time.Duration(100+400*i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	page := func(after *string) []*persistence.TaskMessage {
		t.Helper()
		got, err := msgs.List(ctx, persistence.TaskMessageFilter{TaskID: taskID, After: after, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	p1 := page(nil)
	if len(p1) != 1 || p1[0].ID != first {
		t.Fatalf("page 1 = %v, want the earlier message %s", p1, first)
	}
	p2 := page(&p1[0].ID)
	if len(p2) != 1 || p2[0].ID != second {
		t.Fatalf("page 2 after %s = %v, want %s: a same-second message with a lower id was skipped", first, p2, second)
	}
	if p3 := page(&p2[0].ID); len(p3) != 0 {
		t.Fatalf("page 3 = %v, want none", p3)
	}
	// A cursor naming no message of the task falls back to id > After.
	unknown := taskID + "-m"
	if got := page(&unknown); len(got) != 1 || got[0].ID != first {
		t.Fatalf("an unknown cursor %q returned %v, want %s (id > After)", unknown, got, first)
	}
}

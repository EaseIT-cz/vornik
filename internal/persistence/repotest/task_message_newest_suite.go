package repotest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunTaskMessageNewestSuite is the contract for TaskMessageFilter.Newest
// (GitHub #464, task-lifecycle-conversational design §18): with a limit it
// returns the NEWEST rows, still ordered oldest first, alone and combined with
// MessageKinds. Without it, the oldest rows, as before. taskID must be unique
// per run; the suite creates the task.
func RunTaskMessageNewestSuite(t *testing.T, tasks persistence.TaskRepository, msgs persistence.TaskMessageRepository, taskID string) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	wf := "wf"
	if err := tasks.Create(ctx, &persistence.Task{
		ID: taskID, ProjectID: "p464", WorkflowID: &wf, Status: persistence.TaskStatusRunning,
		CreationSource: persistence.TaskCreationSourceUser, Priority: 50, Payload: []byte(`{}`),
		CreatedAt: base, UpdatedAt: base, MaxAttempts: 1, Attempt: 1,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 12 messages, one a minute; every third is a directive.
	for i := 0; i < 12; i++ {
		kind := persistence.TaskMessageKindMessage
		if i%3 == 2 {
			kind = persistence.TaskMessageKindDirective
		}
		if err := msgs.Insert(ctx, &persistence.TaskMessage{
			ID: fmt.Sprintf("%s-m%02d", taskID, i), TaskID: taskID,
			AuthorKind: persistence.TaskMessageAuthorOperator, MessageKind: kind,
			Content: fmt.Sprintf("message %d", i), CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	ids := func(rows []*persistence.TaskMessage) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID[len(taskID)+1:])
		}
		return out
	}
	check := func(name string, f persistence.TaskMessageFilter, want ...string) {
		t.Helper()
		got, err := msgs.List(ctx, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if g := fmt.Sprint(ids(got)); g != fmt.Sprint(want) {
			t.Fatalf("%s: got %s, want %v", name, g, want)
		}
	}
	check("oldest (unchanged)", persistence.TaskMessageFilter{TaskID: taskID, Limit: 3}, "m00", "m01", "m02")
	check("newest", persistence.TaskMessageFilter{TaskID: taskID, Limit: 3, Newest: true}, "m09", "m10", "m11")
	check("newest directive", persistence.TaskMessageFilter{TaskID: taskID, Limit: 1, Newest: true,
		MessageKinds: []string{persistence.TaskMessageKindDirective}}, "m11")
	check("newest two directives", persistence.TaskMessageFilter{TaskID: taskID, Limit: 2, Newest: true,
		MessageKinds: []string{persistence.TaskMessageKindDirective}}, "m08", "m11")
}

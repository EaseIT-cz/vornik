package chat

import (
	"context"
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #499 (operator decision A19): a chat-created task carries the
// channel's attribution of the turn, and no model text can set it.
func TestExecuteCreateTask_StampsTheChannelsPrincipal(t *testing.T) {
	var created *persistence.Task
	tr := &mocks.MockTaskRepository{CreateFunc: func(_ context.Context, task *persistence.Task) error { created = task; return nil }}
	er := &mocks.MockExecutionRepository{}
	act := Action{Type: ActionCreateTask, Project: "p", Type_: "x", CreatorActor: "user:acct-op"}
	if res, err := ExecuteAction(context.Background(), act, tr, er, nil); err != nil || !res.Success {
		t.Fatalf("create_task: %v %+v", err, res)
	}
	if created == nil || created.CreatedByActor == nil || *created.CreatedByActor != "user:acct-op" {
		t.Fatalf("created_by_actor = %v", created.CreatedByActor)
	}
	created = nil
	if res, err := ExecuteAction(context.Background(), Action{Type: ActionCreateTask, Project: "p", Type_: "x"}, tr, er, nil); err != nil || !res.Success {
		t.Fatalf("create_task: %v %+v", err, res)
	}
	if created.CreatedByActor != nil {
		t.Fatalf("an unattributed turn stamped %q", *created.CreatedByActor)
	}
	// The model's own action JSON cannot name a principal.
	var decoded Action
	if err := json.Unmarshal([]byte(`{"action":"create_task","project":"p","CreatorActor":"user:x","creator_actor":"user:x"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CreatorActor != "" {
		t.Fatalf("model JSON set the principal to %q", decoded.CreatorActor)
	}
}

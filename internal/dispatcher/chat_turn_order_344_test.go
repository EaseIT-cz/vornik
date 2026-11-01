package dispatcher

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// GitHub #344, slice 5d-1 (design 2026-07-29-art17-redact-and-reembed §15.14,
// review e671 M2). Transcript erasure locks a chat turn's task subtree and
// argues that no NEW origin task can appear for an audit row that already
// exists: tasks.chat_turn_id is written mid-turn with the pre-allocated id,
// and the audit row is inserted only when the turn finishes. This drives the
// real turn path (newChatAuditTurn, the context id, create_task, finish) and
// asserts that order, so a change that inserted the audit row first, or
// minted a fresh id at finish, fails here.
func TestChatTurn_OriginTaskPrecedesItsAuditRow(t *testing.T) {
	audits := newStubChatAuditRepo()
	a := &Agent{logger: zerolog.Nop(), chatAuditRepo: audits}
	turn := newChatAuditTurn(a)
	if turn == nil {
		t.Fatal("no audit turn")
	}
	ctx := WithChatTurnID(context.Background(), turn.id)

	reg := mustRegistry(t, []registry.Project{{
		ID: "snake", SwarmID: "s", DefaultWorkflowID: "w",
		Autonomy: registry.ProjectAutonomy{AllowedTaskTypes: []string{"research"}},
	}}, oneSwarm("s"), oneWorkflow("w"))
	tasks := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := newExecutor(withRegistry(reg), withTaskRepo(tasks))
	if res := te.createTask(ctx, `{"prompt":"book it"}`, "snake", nil, 0); strings.Contains(res.Content, "Error:") {
		t.Fatalf("create_task: %s", res.Content)
	}
	if tasks.last == nil || tasks.last.ChatTurnID == nil || *tasks.last.ChatTurnID != turn.id {
		t.Fatalf("origin task chat_turn_id = %v, want %s", tasks.last, turn.id)
	}
	if len(audits.entries) != 0 {
		t.Fatal("the audit row exists before the turn finished; an erasure could then miss an origin task")
	}

	turn.captureRequest("sys", "book it", "lead")
	turn.finish(context.Background(), Request{ChatID: 7, Project: "snake"}, Result{Text: "booked"})
	if len(audits.entries) != 1 || audits.entries[0].ID != turn.id {
		t.Fatalf("audit rows = %+v, want one with id %s", audits.entries, turn.id)
	}
}

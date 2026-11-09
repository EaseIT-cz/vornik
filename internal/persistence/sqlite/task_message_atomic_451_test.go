package sqlite

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #451 (storage-abstraction design, SQLite caveats, 2026-11-08).
// Found at review (code audit). Insert only opened its transaction when the
// handle was a raw *sql.DB; under the metrics-wrapped handle the message
// INSERT and the tasks counter UPDATE ran as two autocommit statements, so a
// failed UPDATE left the message row behind with a stale message_count and
// open_checkpoint_id. A trigger makes the UPDATE fail deterministically.
func TestTaskMessageInsert451_WrappedHandleIsAtomic(t *testing.T) {
	ctx := context.Background()
	db, err := Connect(ctx, DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	task := &persistence.Task{ID: "task-451", ProjectID: "proj-451", Priority: 50, Payload: []byte(`{}`), Status: persistence.TaskStatusQueued}
	if err := NewTaskRepository(db.DB).Create(ctx, task); err != nil {
		t.Fatalf("Create task: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_bump_451 BEFORE UPDATE OF message_count ON tasks
		BEGIN SELECT RAISE(ABORT, 'counter update refused'); END`); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	msgs := NewTaskMessageRepository(persistence.NewDBWithMetrics(db.DB, nil, ""))
	err = msgs.Insert(ctx, &persistence.TaskMessage{
		ID: "msg-451", TaskID: task.ID, AuthorKind: "agent",
		MessageKind: persistence.TaskMessageKindCheckpoint, Content: "which branch?", CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("Insert must fail when the counter update fails")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM task_messages WHERE id = 'msg-451'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the message row survived a failed counter update (%d row): Insert was not one transaction under the wrapped handle", n)
	}
}

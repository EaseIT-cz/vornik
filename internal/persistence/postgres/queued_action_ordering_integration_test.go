//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
	"vornik.io/vornik/internal/persistence"
)

// #119 Stage and Begin must retain the same epoch locks through action INSERT.
// Outer transactions expose the actual retained PostgreSQL locks; no runtime hook.
func TestQueuedActionStageBeginOrdering(t *testing.T) {
	for _, scope := range []string{"ns:queue", persistence.AccountDaemonScope} {
		t.Run(scope, func(t *testing.T) {
			db := newIntegrationDB(t)
			ctx := context.Background()
			now := time.Now().UTC()
			epochs := NewAccountTransitionRepository(db.DB)
			tx, err := db.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			a := &persistence.BrokerAction{ProjectID: "queue--mail", TaskID: persistence.GenerateID("ordering"), WorkflowID: "reply", ActionKind: "send", Tool: "mcp__mail__send", Status: persistence.BrokerActionStaged, ArgsJSON: []byte(`{}`), ArgsSHA256: "hash", ExpiresAt: now.Add(time.Hour)}
			if ok, e := NewBrokerActionRepository(tx).Stage(ctx, a); e != nil || !ok {
				t.Fatalf("Stage %v %v", ok, e)
			}
			deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			if _, e := epochs.Begin(deadline, scope, "racer", "rotate", now); e == nil {
				t.Fatal("Begin bypassed Stage transaction's epoch locks")
			}
			if !errors.Is(deadline.Err(), context.DeadlineExceeded) {
				t.Fatalf("Begin failed without waiting for retained locks: context=%v", deadline.Err())
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			ticket, e := epochs.Begin(ctx, scope, "racer", "rotate", now)
			if e != nil {
				t.Fatal(e)
			}
			if e = epochs.Abort(ctx, ticket); e != nil {
				t.Fatal(e)
			}
			repo := NewBrokerActionRepository(db.DB)
			if _, e = repo.PromoteStaged(ctx, a.TaskID); e != nil {
				t.Fatal(e)
			}
			if e = repo.Approve(ctx, a.ActionID, a.ArgsSHA256, "device", now); !errors.Is(e, persistence.ErrAccountTransition) {
				t.Fatalf("Stage-first stale terminal %v", e)
			}
			ticket, e = epochs.Begin(ctx, scope, "first", "rotate", now)
			if e != nil {
				t.Fatal(e)
			}
			b := *a
			b.ActionID = ""
			b.TaskID = persistence.GenerateID("begin-first")
			if _, e = repo.Stage(ctx, &b); !errors.Is(e, persistence.ErrAccountTransition) {
				t.Fatalf("Begin-first busy Stage %v", e)
			}
			if e = epochs.Abort(ctx, ticket); e != nil {
				t.Fatal(e)
			}
		})
	}
}

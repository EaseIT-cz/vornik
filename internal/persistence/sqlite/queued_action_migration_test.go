package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// #119 upgrade preserves legacy proposals without manufacturing consent epochs.
func TestMigration222_IsAdditive(t *testing.T) {
	db := sqlitetest.Memory(t)
	ctx := context.Background()
	now := time.Now().UTC()
	repo := sqlite.NewBrokerActionRepository(db.DB)
	a := &persistence.BrokerAction{ActionID: "legacy", ProjectID: "operator", TaskID: "task", WorkflowID: "reply", ActionKind: "send", Tool: "mcp__mail__send", Status: persistence.BrokerActionStaged, ArgsJSON: []byte(`{}`), ArgsSHA256: "hash", ExpiresAt: now.Add(time.Hour)}
	if _, err := repo.Stage(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PromoteStaged(ctx, a.TaskID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"account_ns_epoch", "account_daemon_epoch"} {
		if _, err := db.Exec(`ALTER TABLE broker_actions DROP COLUMN ` + name); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.Get(ctx, a.ActionID)
	if err != nil || got.ProjectID != "operator" || got.ArgsSHA256 != "hash" || got.Status != persistence.BrokerActionPending || got.AccountGuard != nil {
		t.Fatalf("legacy changed: %+v %v", got, err)
	}
	if _, err = db.Exec(`UPDATE broker_actions SET project_id='hermes--mail' WHERE action_id='legacy'`); err != nil {
		t.Fatal(err)
	}
	if err = repo.Approve(ctx, a.ActionID, a.ArgsSHA256, "device", now); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("NULL legacy agent approval=%v", err)
	}
	for _, q := range []string{`UPDATE broker_actions SET account_ns_epoch=0`, `UPDATE broker_actions SET account_daemon_epoch=0,account_ns_epoch=NULL`} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
		if err = repo.Approve(ctx, a.ActionID, a.ArgsSHA256, "device", now); !errors.Is(err, persistence.ErrAccountTransition) {
			t.Fatalf("partial legacy approval=%v", err)
		}
	}
	if _, err = db.Exec(`UPDATE broker_actions SET account_ns_epoch=-1`); err == nil {
		t.Fatal("negative epoch admitted")
	}
}

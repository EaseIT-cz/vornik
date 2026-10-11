//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
	"vornik.io/vornik/internal/persistence"
)

// #119 upgrade: additive migration never backfills a legacy agent action.
func TestMigration222_IsAdditive(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{`CREATE TEMP TABLE broker_actions (LIKE public.broker_actions INCLUDING ALL) ON COMMIT DROP`, `SET LOCAL search_path=pg_temp`, `ALTER TABLE broker_actions DROP COLUMN account_ns_epoch`, `ALTER TABLE broker_actions DROP COLUMN account_daemon_epoch`, `INSERT INTO broker_actions(action_id,project_id,task_id,workflow_id,action_kind,tool,args_json,args_sha256,status,created_at,expires_at) VALUES('legacy','hermes--mail','task','reply','send','mcp__mail__send','{}','hash','pending',NOW(),NOW()+INTERVAL '1 hour')`} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var up string
	for _, m := range persistence.DefaultMigrations {
		if m.Version == 222 {
			up = m.Up
		}
	}
	if up == "" {
		t.Fatal("migration222 missing")
	}
	for range 2 {
		if _, err = tx.Exec(up); err != nil {
			t.Fatal(err)
		}
	}
	repo := NewBrokerActionRepository(tx)
	a, err := repo.Get(ctx, "legacy")
	if err != nil || a.AccountGuard != nil || a.Status != persistence.BrokerActionPending || a.ArgsSHA256 != "hash" {
		t.Fatalf("legacy changed: %+v %v", a, err)
	}
	if err = repo.Approve(ctx, a.ActionID, a.ArgsSHA256, "device", time.Now()); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("legacy approval=%v", err)
	}
	for _, q := range []string{`UPDATE broker_actions SET account_ns_epoch=0,account_daemon_epoch=NULL`, `UPDATE broker_actions SET account_ns_epoch=NULL,account_daemon_epoch=0`} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(err)
		}
		if err = repo.Approve(ctx, a.ActionID, a.ArgsSHA256, "device", time.Now()); !errors.Is(err, persistence.ErrAccountTransition) {
			t.Fatalf("partial legacy terminal %v", err)
		}
	}
	if _, err = tx.Exec(`UPDATE broker_actions SET project_id='operator',account_ns_epoch=NULL,account_daemon_epoch=NULL`); err != nil {
		t.Fatal(err)
	}
	if err = repo.Approve(ctx, a.ActionID, a.ArgsSHA256, "operator", time.Now()); err != nil {
		t.Fatalf("preserved legacy operator approval %v", err)
	}
	preserved, e := repo.Get(ctx, a.ActionID)
	if e != nil || preserved.Status != persistence.BrokerActionApproved || preserved.AccountGuard != nil || preserved.ArgsSHA256 != "hash" {
		t.Fatalf("operator preservation %+v %v", preserved, e)
	}

}

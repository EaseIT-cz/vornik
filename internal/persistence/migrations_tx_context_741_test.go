package persistence

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// GitHub #741 (storage-abstraction design, amendment 2026-11-12): a
// migration's Tx was begun on Run's context. When that context ended,
// database/sql's Tx watcher (awaitDone) rolled back with a discard on its own
// goroutine, which closes the lock's *sql.Conn (dc = nil). If that landed
// between Conn.grabConn's done check and its lock, releaseMigrationConn's
// unlock ran on a nil driver connection and panicked. Beginning the Tx on a
// context that never ends by itself leaves every close on the runner's
// goroutine; the statements still run on Run's context, so a deadline still
// cancels them.
//
// Found at: unit (make test -race lane, 2026-11-11, one panic in
// TestMigrationRunner_Run_ErrorNamesTheEndedContext).
// Regression test at: unit TestApplyMigration741_TxContextNeverEndsByItself.
// The race itself is inside database/sql; this pins the property that removes
// it, and the statement test pins that a deadline still cancels.

type recordingBeginner struct {
	*sql.DB
	txCtx context.Context
}

func (r *recordingBeginner) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	r.txCtx = ctx
	return r.DB.BeginTx(ctx, opts)
}

func TestApplyMigration741_TxContextNeverEndsByItself(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO migrations").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	q := &recordingBeginner{DB: db}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := NewMigrationRunnerFor(db, nil)
	if err := r.applyMigration(ctx, q, Migration{Version: 2, Name: "a", Up: "CREATE TABLE a"}); err != nil {
		t.Fatal(err)
	}
	if q.txCtx == nil {
		t.Fatal("BeginTx was not called")
	}
	if q.txCtx.Done() != nil {
		t.Fatal("the migration Tx was begun on a context that can end on its own; its watcher would close the lock's connection off the runner's goroutine")
	}
}

func TestApplyMigration741_DeadlineStillCancelsTheStatement(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE b").WillDelayFor(5 * time.Second).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := NewMigrationRunnerFor(db, nil)
	start := time.Now()
	err = r.applyMigration(ctx, db, Migration{Version: 3, Name: "b", Up: "CREATE TABLE b"})
	if err == nil {
		t.Fatal("a statement past the deadline succeeded")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("the deadline did not cancel the statement (waited %s)", waited)
	}
}

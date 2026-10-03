package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// TestConnect_WriteTransactionsBeginImmediate pins the begin mode the code
// relies on. LeaseTask, IngestQueue.ClaimBatch and the corpus epoch swap each
// read candidates and then write, and each says it runs under BEGIN
// IMMEDIATE so that a concurrent writer waits instead of racing it.
// modernc.org/sqlite ignores TxOptions.Isolation, and only the _txlock DSN
// parameter selects the begin mode; without it every transaction was
// DEFERRED (found 2026-10-02 while proving the approver first-device race).
//
// The probe: transaction A opens and only reads. Under IMMEDIATE, A already
// holds the writer lock, so a write from another connection waits until A
// ends. Under DEFERRED it completes at once.
func TestConnect_WriteTransactionsBeginImmediate(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Connect(t, filepath.Join(t.TempDir(), "txlock.db"))
	if _, err := db.ExecContext(ctx, `CREATE TABLE probe (v INTEGER)`); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	wrote := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, `INSERT INTO probe (v) VALUES (1)`)
		wrote <- err
	}()
	select {
	case err := <-wrote:
		_ = tx.Rollback()
		t.Fatalf("a concurrent write completed while a write transaction was open (err=%v): transactions begin DEFERRED", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the waiting write failed after the transaction ended: %v", err)
	}
}

// A read-only transaction must not take the writer lock: the driver leaves
// ReadOnly transactions DEFERRED whatever _txlock says, so readers stay
// concurrent with writers.
func TestConnect_ReadOnlyTransactionsDoNotBlockWriters(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Connect(t, filepath.Join(t.TempDir(), "txlock-ro.db"))
	if _, err := db.ExecContext(ctx, `CREATE TABLE probe (v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	ro, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Rollback() }()
	var n int
	if err := ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, `INSERT INTO probe (v) VALUES (1)`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a read-only transaction blocked a writer")
	}
}

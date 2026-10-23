package persistence

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// GitHub #424: a migration cancelled by its caller's context failed as
// "failed to apply migration 13 (...): migration SQL failed: pq: canceling
// statement due to user request", which names neither the context nor how
// far the chain got. The integration-level regression test is
// test/integration's TestIntegration_MigrationBudget_NamesTheCanceller;
// these pin the runner's half without a server.

func expectRunUpToPending(mock sqlmock.Sqlmock) {
	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
}

func TestMigrationRunner_Run_ErrorNamesTheEndedContext(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectRunUpToPending(mock)
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO migrations").WithArgs(2, "a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE b").WillDelayFor(5 * time.Second).WillReturnResult(sqlmock.NewResult(0, 0))

	runner := NewMigrationRunnerFor(db, []Migration{
		{Version: 1, Name: "initial", Up: "SELECT 1"},
		{Version: 2, Name: "a", Up: "CREATE TABLE a"},
		{Version: 3, Name: "b", Up: "CREATE TABLE b"},
	})
	budget := errors.New("the boot budget of 200ms ran out")
	ctx, cancel := context.WithTimeoutCause(context.Background(), 200*time.Millisecond, budget)
	defer cancel()

	err = runner.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil; want the ended context's error")
	}
	msg := err.Error()
	for _, want := range []string{
		"failed to apply migration 3 (b)",
		"the context ended",
		"after Run started, with 1 of 2 pending migrations applied",
		"the boot budget of 200ms ran out",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if !errors.Is(err, budget) {
		t.Errorf("errors.Is(err, cause) = false for %q", msg)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false for %q", msg)
	}
}

// A caller that gives no cause still learns a context ended, and when.
func TestMigrationRunner_Run_CauselessContextIsNamed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectRunUpToPending(mock)
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE a").WillDelayFor(5 * time.Second).WillReturnResult(sqlmock.NewResult(0, 0))

	runner := NewMigrationRunnerFor(db, []Migration{
		{Version: 1, Name: "initial", Up: "SELECT 1"},
		{Version: 2, Name: "a", Up: "CREATE TABLE a"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = runner.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "with 0 of 1 pending migrations applied: context deadline exceeded") {
		t.Errorf("error %q does not name the ended context", msg)
	}
	if strings.Count(msg, "context deadline exceeded") != 1 {
		t.Errorf("error %q repeats the context error", msg)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false for %q", msg)
	}
}

func TestMigrationRunner_Run_LiveContextKeepsTheError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectRunUpToPending(mock)
	mock.ExpectBegin()
	mock.ExpectExec("CREATE TABLE a").WillReturnError(errors.New("syntax error"))
	mock.ExpectRollback()
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunnerFor(db, []Migration{
		{Version: 1, Name: "initial", Up: "SELECT 1"},
		{Version: 2, Name: "a", Up: "CREATE TABLE a"},
	})
	err = runner.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil")
	}
	if got, want := err.Error(), "failed to apply migration 2 (a): migration SQL failed: syntax error"; got != want {
		t.Errorf("error = %q, want %q (unchanged while the context is live)", got, want)
	}
}

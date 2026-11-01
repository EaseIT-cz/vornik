package persistence

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// GitHub #581 D1 (enterprise-packaging-design.md, amendment 2026-10-29): a
// database migrated by a newer release refuses the start of an older binary,
// before anything is applied, instead of the older binary running silently
// on a schema it was not built for.
func TestMigrationRunner_Run_RefusesASchemaNewerThanTheBinary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// Applied: 1 and 7. The binary knows 1 to 3, so 2 and 3 are pending,
	// and neither may be applied once 7 is seen.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1).AddRow(7))
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunnerFor(db, []Migration{
		{Version: 1, Name: "initial", Up: "SELECT 1"},
		{Version: 2, Name: "two", Up: "SELECT 2"},
		{Version: 3, Name: "three", Up: "SELECT 3"},
	})
	err = runner.Run(context.Background())
	if !errors.Is(err, ErrSchemaNewerThanBinary) {
		t.Fatalf("Run = %v; want ErrSchemaNewerThanBinary", err)
	}
	for _, want := range []string{"7", "3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name version %s", err, want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a migration was applied or the lock not released: %v", err)
	}
}

// An applied version below the binary's newest that the binary does not list
// (a migration renumbered or removed long ago) is not a newer schema: D1
// refuses only a database ahead of the binary.
func TestMigrationRunner_Run_AnUnknownOldVersionIsNotANewerSchema(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1).AddRow(2).AddRow(3))
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunnerFor(db, []Migration{
		{Version: 1, Name: "initial", Up: "SELECT 1"},
		{Version: 3, Name: "three", Up: "SELECT 3"},
	})
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run = %v; want nil", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations unmet: %v", err)
	}
}

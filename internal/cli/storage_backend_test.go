package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
)

func TestRequirePostgresDBRejectsSQLiteBackend(t *testing.T) {
	_, err := requirePostgresDB(&storage.Backend{Driver: "sqlite"}, "memory reassign")
	if err == nil {
		t.Fatal("expected unsupported-backend error")
	}
	if !strings.Contains(err.Error(), "requires database.driver=postgres") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequirePostgresDBReturnsPostgresHandle(t *testing.T) {
	db := &sql.DB{}
	got, err := requirePostgresDB(&storage.Backend{
		Driver: "postgres",
		PG:     &postgres.DB{DB: db},
	}, "retention")
	if err != nil {
		t.Fatalf("requirePostgresDB: %v", err)
	}
	if got != db {
		t.Fatalf("got %p, want %p", got, db)
	}
}

// audit 2026-10-13 (GitHub #360): `vornikctl retention` refused SQLite while
// the daemon ran the same sweeper there. Its statements are driver-correct now,
// so the CLI opens either driver; Postgres still goes through requirePostgresDB.
func TestRetentionDBOpensBothDrivers(t *testing.T) {
	lite := sqlitetest.Open(t, ":memory:").DB
	got, err := retentionDB(&storage.Backend{Driver: "sqlite", DB: lite})
	if err != nil || got != lite {
		t.Fatalf("retentionDB(sqlite) = %p, %v; want the sqlite handle", got, err)
	}
	pg := &sql.DB{}
	got, err = retentionDB(&storage.Backend{Driver: "postgres", PG: &postgres.DB{DB: pg}})
	if err != nil || got != pg {
		t.Fatalf("retentionDB(postgres) = %p, %v; want the postgres handle", got, err)
	}
	for _, b := range []*storage.Backend{nil, {Driver: "mysql"}, {Driver: "sqlite"}, {Driver: "postgres"}} {
		if _, err := retentionDB(b); err == nil {
			t.Fatalf("retentionDB(%+v) must refuse", b)
		}
	}
}

// audit 2026-10-12 (GitHub #355): every `vornikctl subject` verb refused SQLite,
// so on Community's default backend a data-subject request could not even be
// recorded. The store and ledger verbs now open it on either driver.
func TestSubjectStoreOpensOnSQLite(t *testing.T) {
	db := sqlitetest.File(t, "subjects.db")
	repo, err := subjectStoreFor(&storage.Backend{Driver: "sqlite", DB: db.DB}, "subject")
	if err != nil {
		t.Fatalf("subjectStoreFor(sqlite): %v", err)
	}
	ctx := context.Background()
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: "ds_cli", DisplayName: "CLI"}); err != nil {
		t.Fatalf("CreateSubject on SQLite: %v", err)
	}
	if _, err := subjectStoreFor(&storage.Backend{Driver: "mysql"}, "subject"); err == nil {
		t.Fatal("an unknown driver must be refused")
	}
	if _, err := subjectStoreFor(nil, "subject"); err == nil {
		t.Fatal("a nil backend must be refused")
	}
}

// The verbs whose machinery is Postgres-only refuse SQLite by name, and say the
// ledger still works there, so the operator knows the request can be recorded.
func TestSubjectExecutorVerbsRefuseSQLiteByName(t *testing.T) {
	_, err := requirePostgresSubjectVerb(&storage.Backend{Driver: "sqlite"}, "subject erase",
		"the artifact cascade and chunk redaction")
	if err == nil {
		t.Fatal("subject erase must refuse SQLite")
	}
	for _, want := range []string{"subject erase requires database.driver=postgres (current: sqlite)",
		"the artifact cascade and chunk redaction", "the request ledger itself works on SQLite"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}

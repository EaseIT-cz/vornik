package cli

import (
	"database/sql"
	"fmt"

	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/subjectstore"
	"vornik.io/vornik/internal/storage"
)

func requirePostgresDB(backend *storage.Backend, feature string) (*sql.DB, error) {
	if backend == nil {
		return nil, fmt.Errorf("%s requires a postgres database backend", feature)
	}
	if backend.Driver != "postgres" {
		return nil, fmt.Errorf("%s requires database.driver=postgres (current: %s)", feature, backend.Driver)
	}
	if backend.PG == nil || backend.PG.DB == nil {
		return nil, fmt.Errorf("%s requires an initialized postgres database backend", feature)
	}
	return backend.PG.DB, nil
}

// retentionDB returns the database handle of whichever driver the backend
// runs, for a feature whose SQL is driver-correct on both: the retention
// sweeper since GitHub #360 (tool-audit retention floor design §4.2 D7). The
// Postgres path is requirePostgresDB, unchanged.
func retentionDB(backend *storage.Backend) (*sql.DB, error) {
	const feature = "retention"
	if backend == nil {
		return nil, fmt.Errorf("%s requires a database backend", feature)
	}
	switch backend.Driver {
	case "postgres":
		return requirePostgresDB(backend, feature)
	case "sqlite":
		if backend.DB == nil {
			return nil, fmt.Errorf("%s requires an initialized sqlite database backend", feature)
		}
		return backend.DB, nil
	default:
		return nil, fmt.Errorf("%s: unsupported database driver %q", feature, backend.Driver)
	}
}

// subjectStoreFor opens the GDPR data-subject store on whichever driver the
// backend runs. The store, the rights ledger and the Art 18 record work on both
// since GitHub #355 (GDPR design §4.19): an Art 12(3) clock does not depend on
// the driver.
func subjectStoreFor(backend *storage.Backend, feature string) (*subjectstore.Repository, error) {
	if backend == nil {
		return nil, fmt.Errorf("%s requires a database backend", feature)
	}
	switch backend.Driver {
	case "postgres":
		db, err := requirePostgresDB(backend, feature)
		if err != nil {
			return nil, err
		}
		return postgres.NewDataSubjectRepository(db), nil
	case "sqlite":
		if backend.DB == nil {
			return nil, fmt.Errorf("%s requires an initialized sqlite database backend", feature)
		}
		return sqlite.NewDataSubjectRepository(backend.DB), nil
	default:
		return nil, fmt.Errorf("%s: unsupported database driver %q", feature, backend.Driver)
	}
}

// requirePostgresSubjectVerb is requirePostgresDB for a subject verb whose
// machinery is Postgres-only (GDPR design §4.19.4). The refusal names that
// machinery and says the ledger itself works, so an operator on SQLite knows
// the request can still be recorded, verified, refused and its deadline
// tracked — only its execution by the software is unavailable.
func requirePostgresSubjectVerb(backend *storage.Backend, verb, machinery string) (*sql.DB, error) {
	db, err := requirePostgresDB(backend, verb)
	if err != nil && backend != nil && backend.Driver != "postgres" {
		return nil, fmt.Errorf("%w: %s is Postgres-only; the request ledger itself works on SQLite — "+
			"record, verify, refuse and track the request with 'vornikctl subject'", err, machinery)
	}
	return db, err
}

package service

import (
	"testing"

	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #365 (2026-10-13): the archive sweeper wired the Postgres deleter on
// whatever c.DB was, so on SQLite every project wipe failed at
// `no such table: memory_embed_queue` and deleted nothing.
func TestProjectDataDeleterFor_PicksTheDriversDeleter(t *testing.T) {
	if _, ok := projectDataDeleterFor("sqlite", nil).(*sqlite.ProjectDataCleanupRepository); !ok {
		t.Error("driver sqlite must get the SQLite deleter")
	}
	for _, d := range []string{"postgres", ""} {
		if _, ok := projectDataDeleterFor(d, nil).(*postgres.ProjectDataCleanupRepository); !ok {
			t.Errorf("driver %q must get the Postgres deleter", d)
		}
	}
}

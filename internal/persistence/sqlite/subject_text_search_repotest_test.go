package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #345 (2026-10-12): the §5.1 free-text backstop had no implementation;
// the console resolution page searches through SQLSubjectTextSearch, whose one
// statement must answer identically on both drivers.
func TestSubjectTextSearch_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunSubjectTextSearchSuite(t, persistence.NewSQLSubjectTextSearch(db.DB),
		seedChunkForSuite(db), restrictChunkForSuite(db), linkChunkForSuite(db))
}

// linkChunkForSuite seeds a link row directly: SQLite has no subject writer
// (design §4.16.3), only the tables the shared predicate and the search read.
func linkChunkForSuite(db *sqlite.DB) repotest.LinkChunk {
	return func(ctx context.Context, subjectID, chunkID, projectID, source string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO data_subject_links
			(subject_id, table_name, row_id, project_id, source, confidence, exclusivity, created_at)
			VALUES (?, 'project_memory_chunks', ?, ?, ?, 'possible', 'unknown', ?)`,
			subjectID, chunkID, projectID, source, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}
}

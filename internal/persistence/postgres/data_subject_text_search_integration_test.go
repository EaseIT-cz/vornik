//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #345 (2026-10-12): the §5.1 free-text backstop. Same suite as the
// SQLite file; a disagreement between the two is the finding.
func TestSubjectTextSearch_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunSubjectTextSearchSuite(t, persistence.NewSQLSubjectTextSearch(db.DB),
		seedChunkForSuite(db), restrictChunkForSuite(db), linkChunkForSuite(db))
}

// GitHub #345: the production lane folds Unicode case (SQLite's LOWER folds
// ASCII only, so the shared suite asserts ASCII). A request naming "Müller"
// must find a record that wrote "MÜLLER".
func TestIntegrationSubjectTextSearch_FoldsUnicodeOnPostgres(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	project := "p345-unicode"
	_, _ = db.DB.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id = $1`, project)
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM project_memory_chunks WHERE project_id = $1`, project)
	})
	seedChunkInProject(t, db, "chunk-345-umlaut", project, "Brief von SØREN MÜLLER erhalten")
	got, err := persistence.NewSQLSubjectTextSearch(db.DB).FindMentions(ctx,
		persistence.SubjectTextQuery{SubjectID: "ds_none", Term: "Søren Müller", ProjectID: project, Limit: 10})
	if err != nil {
		t.Fatalf("FindMentions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want the upper-cased Unicode name found on Postgres, got %d rows", len(got))
	}
}

// linkChunkForSuite creates the subject (the link's foreign key) and the link.
func linkChunkForSuite(db *DB) repotest.LinkChunk {
	return func(ctx context.Context, subjectID, chunkID, projectID, source string) error {
		if _, err := db.DB.ExecContext(ctx, `INSERT INTO data_subjects (id, display_name)
			VALUES ($1, 'suite') ON CONFLICT (id) DO NOTHING`, subjectID); err != nil {
			return err
		}
		_, err := db.DB.ExecContext(ctx, `INSERT INTO data_subject_links
			(subject_id, table_name, row_id, project_id, source, confidence, exclusivity)
			VALUES ($1, 'project_memory_chunks', $2, $3, $4, 'possible', 'unknown')`,
			subjectID, chunkID, projectID, source)
		return err
	}
}

// GitHub #345: the console can undo an operator's own mistaken confirm before an
// erasure executes, and ONLY that — a binder-derived link is not removable here.
func TestIntegrationDataSubject_DeleteOperatorLinkRemovesOnlyOperatorAsserted(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewDataSubjectRepository(db.DB)
	const subject = "ds_unlink_345"
	_, _ = db.DB.ExecContext(ctx, `DELETE FROM data_subjects WHERE id = $1`, subject)
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: subject, DisplayName: "Unlink Test"}); err != nil {
		t.Fatalf("CreateSubject: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM data_subjects WHERE id = $1`, subject)
	})
	add := func(row string, src datasubject.Source, conf datasubject.Confidence) {
		t.Helper()
		if err := repo.AddLink(ctx, subject, datasubject.Link{Table: datasubject.TableProjectMemoryChunks, RowID: row,
			ProjectID: "p345", Source: src, Confidence: conf, Exclusivity: datasubject.UnknownExclusivity}); err != nil {
			t.Fatalf("AddLink %s: %v", row, err)
		}
	}
	add("chunk-op", datasubject.SourceOperatorAsserted, datasubject.ConfidenceProbable)
	add("chunk-kg", datasubject.SourceKGExtraction, datasubject.ConfidencePossible)

	// AddLinkIfAbsent never overwrites: confirming a row a binder linked
	// meanwhile must not leave a kg_extraction link claiming `probable`.
	inserted, err := repo.AddLinkIfAbsent(ctx, subject, datasubject.Link{Table: datasubject.TableProjectMemoryChunks,
		RowID: "chunk-kg", ProjectID: "p345", Source: datasubject.SourceOperatorAsserted,
		Confidence: datasubject.ConfidenceProbable, Exclusivity: datasubject.UnknownExclusivity})
	if err != nil || inserted {
		t.Fatalf("AddLinkIfAbsent over an existing link: inserted=%v err=%v", inserted, err)
	}
	var conf string
	if err := db.DB.QueryRowContext(ctx, `SELECT confidence FROM data_subject_links
		WHERE subject_id = $1 AND row_id = 'chunk-kg'`, subject).Scan(&conf); err != nil || conf != "possible" {
		t.Fatalf("the binder link was modified: confidence=%q err=%v", conf, err)
	}
	if inserted, err := repo.AddLinkIfAbsent(ctx, subject, datasubject.Link{Table: datasubject.TableProjectMemoryChunks,
		RowID: "chunk-new", ProjectID: "p345", Source: datasubject.SourceOperatorAsserted,
		Confidence: datasubject.ConfidenceProbable, Exclusivity: datasubject.UnknownExclusivity}); err != nil || !inserted {
		t.Fatalf("AddLinkIfAbsent on a fresh row: inserted=%v err=%v", inserted, err)
	}
	if ok, err := repo.DeleteOperatorLink(ctx, subject, string(datasubject.TableProjectMemoryChunks), "chunk-new"); err != nil || !ok {
		t.Fatalf("cleanup chunk-new: %v %v", ok, err)
	}

	if ok, err := repo.DeleteOperatorLink(ctx, subject, string(datasubject.TableProjectMemoryChunks), "chunk-kg"); err != nil || ok {
		t.Fatalf("a binder-derived link must not be removable: removed=%v err=%v", ok, err)
	}
	if ok, err := repo.DeleteOperatorLink(ctx, subject, string(datasubject.TableProjectMemoryChunks), "chunk-op"); err != nil || !ok {
		t.Fatalf("the operator's own link must be removable: removed=%v err=%v", ok, err)
	}
	links, err := repo.ListLinks(ctx, subject)
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(links) != 1 || links[0].RowID != "chunk-kg" {
		t.Fatalf("want only the binder link left, got %+v", links)
	}
}

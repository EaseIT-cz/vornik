package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Issue #330 (audit 2026-10-12, dangling-backlog sweep): the classify
// backfill's only skip state was an in-memory cursor, so a restart — and every
// tick that classified anything, which resets the cursor — re-asked the paid
// model about rows it had already abstained on. Design: rag-ingest pipeline
// LLD, "Change, 2026-10-13 (#330)".

func abstainPage() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
		AddRow("c1", "p", "a.md", "dispatcher", "one").
		AddRow("c2", "p", "b.md", "dispatcher", "two")
}

const stampSQL = `UPDATE project_memory_chunks SET classify_attempted_at`

// The auto-loop stamps every abstention, filters stamped rows within the retry
// window, and — because stamped rows leave its result set — does not advance
// its cursor past them. A restarted backfiller (fresh offset) asks for the same
// page and the database, not the cursor, keeps the abstained rows out.
func TestClassifyAcrossProjects_RestartDoesNotReaskAbstainedRows(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "unclassified"}, {content: "unclassified"},
	})
	defer cleanup()
	window := int64(ClassifyRetryAfter.Seconds())
	mock.ExpectQuery(`classify_attempted_at IS NULL OR classify_attempted_at <`).
		WithArgs(2, 0, window).WillReturnRows(abstainPage())
	mock.ExpectExec(stampSQL).WithArgs("c1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(stampSQL).WithArgs("c2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	res, err := bf.BackfillBatchAcrossProjects(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 2 || res.Failed != 0 {
		t.Fatalf("want two stamped abstentions, got %+v", res)
	}
	if bf.offset != 0 {
		t.Fatalf("stamped rows leave the query; cursor = %d, want 0", bf.offset)
	}

	// The restart: a fresh backfiller over the same store. Its query carries
	// the window, so the stamped rows are not returned and the model is not
	// asked (the provider has no replies left; a call would fail the batch).
	restarted, mock2, cleanup2 := newClassifyBackfiller(t, nil)
	defer cleanup2()
	mock2.ExpectQuery(`classify_attempted_at IS NULL OR classify_attempted_at <`).
		WithArgs(2, 0, window).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}))
	mock2.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	res2, err := restarted.BackfillBatchAcrossProjects(context.Background(), 2)
	if err != nil || res2.Processed != 0 {
		t.Fatalf("restarted sweep: %+v %v", res2, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A stamp that fails to persist leaves the row in the query, so it counts as
// Failed and the cursor walks past it (review F4: the load-bearing branch of
// "advance by Failed").
func TestClassifyAcrossProjects_FailedStampStaysAndIsWalkedPast(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "unclassified"}, {content: "unclassified"},
	})
	defer cleanup()
	mock.ExpectQuery(`classify_attempted_at`).WithArgs(2, 0, sqlmock.AnyArg()).WillReturnRows(abstainPage())
	mock.ExpectExec(stampSQL).WithArgs("c1").WillReturnError(errors.New("disk full"))
	mock.ExpectExec(stampSQL).WithArgs("c2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	res, err := bf.BackfillBatchAcrossProjects(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Skipped != 1 {
		t.Fatalf("want 1 failed stamp + 1 stamped abstention, got %+v", res)
	}
	if bf.offset != 1 {
		t.Fatalf("cursor = %d, want 1 (past the unstamped row only)", bf.offset)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The operator's per-project sweep re-looks at everything (no filter) but
// still stamps, and keeps advancing by Processed because stamped rows stay in
// ITS query (review F3: both cursor rules pinned).
func TestClassifyPerProject_StampsButDoesNotFilter(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "unclassified"}, {content: "unclassified"},
	})
	defer cleanup()
	mock.ExpectQuery(`WHERE project_id = \$1\s+AND COALESCE\(content_class, ''\) IN \(''\, 'unclassified'\)\s+AND [^c]`).
		WithArgs("p", 2, 0).WillReturnRows(abstainPage())
	mock.ExpectExec(stampSQL).WithArgs("c1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(stampSQL).WithArgs("c2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("dispatcher", 2))
	res, err := bf.BackfillBatch(context.Background(), "p", 2)
	if err != nil || res.Skipped != 2 {
		t.Fatalf("got %+v %v", res, err)
	}
	if got := bf.projectOffsets["p"]; got != 2 {
		t.Fatalf("per-project cursor = %d, want 2 (Processed)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

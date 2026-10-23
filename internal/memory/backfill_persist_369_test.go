package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
)

// GitHub #369, outcome-quality LLD §S8.11 row 7b: a model failure and a
// persist failure both counted backfill_chunks_total{status="failed"}, so
// "the title model is down" and "the database refused the write" read the
// same. A persist failure is now its own status, and failed keeps only the
// model's failures.

func TestTitleBackfill_APersistFailureIsCountedApart(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, []titlerReply{{content: "Topic"}})
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}).AddRow("c1", "p", "s", "x"))
	mock.ExpectExec("UPDATE project_memory_chunks SET content_title").WillReturnError(errors.New("disk full"))
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	res := bf.runOnce(context.Background(), 5)
	if res == nil || res.Failed != 1 || res.PersistFailed != 1 {
		t.Fatalf("result = %+v, want Failed 1 and PersistFailed 1", res)
	}
	chunks := bf.Metrics.TitleBackfillChunksTotal
	if got := testutil.ToFloat64(chunks.WithLabelValues("persist_failed")); got != 1 {
		t.Fatalf("title_backfill_chunks_total{persist_failed} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(chunks.WithLabelValues("failed")); got != 0 {
		t.Fatalf("title_backfill_chunks_total{failed} = %v, want 0: a persist failure is not the model's", got)
	}
}

func TestClassifyBackfill_PersistFailuresAreCountedApart(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "research"},
		{err: errors.New("upstream 503")},
		{content: "spec"},
	})
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p-a", "alpha.md", "dispatcher", "alpha").
			AddRow("c3", "p-c", "gamma.md", "dispatcher", "gamma").
			AddRow("c4", "p-d", "delta.md", "dispatcher", "delta"))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnError(errors.New("disk full"))
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	res, err := bf.BackfillBatchAcrossProjects(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 2 || res.PersistFailed != 1 {
		t.Fatalf("result = %+v, want Failed 2 (one model, one persist) and PersistFailed 1", res)
	}
}

func TestClassifyBackfill_RoleMapPersistFailureIsCountedApart(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "doc.md", "researcher", "body"))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnError(errors.New("disk full"))
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnRows(sqlmock.NewRows([]string{"role", "n"}))

	res, err := bf.BackfillBatch(context.Background(), "p", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.PersistFailed != 1 {
		t.Fatalf("result = %+v, want Failed 1 and PersistFailed 1", res)
	}
}

func TestBackfillHooks_SplitFailedFromPersistFailed(t *testing.T) {
	m := freshMetrics()
	set := &backfillMetricSet{ticks: m.ClassifyBackfillTicksTotal, chunks: m.ClassifyBackfillChunksTotal, remaining: m.ClassifyBackfillRemainingChunks}
	h := newBackfillHooks("classify", zerolog.Nop(), set, nil, nil)
	h.chunkCounts(backfillCounts{Succeeded: 1, Failed: 3, PersistFailed: 1, Skipped: 2})
	for status, want := range map[string]float64{"succeeded": 1, "failed": 2, "persist_failed": 1, "skipped": 2} {
		if got := testutil.ToFloat64(m.ClassifyBackfillChunksTotal.WithLabelValues(status)); got != want {
			t.Errorf("classify_backfill_chunks_total{%s} = %v, want %v", status, got, want)
		}
	}
}

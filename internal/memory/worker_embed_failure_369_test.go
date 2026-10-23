package memory

import (
	"context"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// GitHub #369 item 5 (outcome-quality §S8.10): the embed worker received (nil,
// nil) for every embedder failure, so the DLQ row said "embedding_failed" with
// an empty last_error, and a provider dimension mismatch (rejected inside the
// embedder before the worker could see the width) was retried on the back-off
// forever instead of parked like the worker's own dimension check parks it.

// containsArg matches a driver argument that contains sub.
type containsArg struct{ sub string }

func (c containsArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, c.sub)
}

func expectDequeue(mock sqlmock.Sqlmock, ids ...string) {
	mock.ExpectQuery("FROM memory_embed_dlq").
		WillReturnRows(sqlmock.NewRows([]string{
			"chunk_id", "project_id", "reason", "last_error",
			"retry_count", "retry_after", "first_failed_at", "last_failed_at",
		}))
	mock.ExpectBegin()
	claimed := sqlmock.NewRows([]string{"chunk_id"})
	chunks := sqlmock.NewRows([]string{
		"id", "project_id", "task_id", "artifact_id", "source_name",
		"chunk_index", "content", "content_hash", "created_at",
	})
	for i, id := range ids {
		claimed.AddRow(id)
		chunks.AddRow(id, "p", "", "", "s", i, "text "+id, "h"+id, time.Now())
	}
	mock.ExpectQuery("UPDATE memory_embed_queue SET claimed_at").WillReturnRows(claimed)
	mock.ExpectQuery("SELECT id, project_id").WillReturnRows(chunks)
	mock.ExpectCommit()
}

func expectDLQ(mock sqlmock.Sqlmock, id, reason, lastErrSub string, park bool) {
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_dlq").
		WithArgs(id, "p", reason, containsArg{lastErrSub}, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if park {
		mock.ExpectExec("UPDATE memory_embed_dlq SET retry_count = -1").
			WithArgs(id).WillReturnResult(sqlmock.NewResult(0, 1))
	}
}

func embedServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestWorker369_ProviderDimensionMismatchIsParkedWithItsWidths(t *testing.T) {
	url := embedServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2,3,4,5]}]}`))
	})
	cfg := Config{EmbeddingEndpoint: url, EmbeddingModel: "m", EmbeddingDimension: 3}
	for _, ids := range [][]string{{"c1"}, {"c1", "c2"}} {
		t.Run(strings.Join(ids, "+"), func(t *testing.T) {
			r, mock, cleanup := newRepo(t)
			defer cleanup()
			w := NewWorker(cfg, r, NewEmbedder(cfg), zerolog.Nop())
			w.setMetrics(freshMetrics())
			expectDequeue(mock, ids...)
			// A batch of 2 fails the batch call (one reply for two inputs is a
			// partial response) and is retried chunk by chunk; each chunk then
			// meets the dimension mismatch.
			for _, id := range ids {
				expectDLQ(mock, id, "dimension_mismatch", "got dim=5, expected=3", true)
			}
			mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, int64(len(ids))))
			w.processBatch(context.Background())
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorker369_EmbedFailureCarriesItsCauseIntoTheDLQ(t *testing.T) {
	url := embedServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	cfg := Config{EmbeddingEndpoint: url, EmbeddingModel: "m", EmbeddingDimension: 3}
	for _, ids := range [][]string{{"c1"}, {"c1", "c2"}} {
		t.Run(strings.Join(ids, "+"), func(t *testing.T) {
			r, mock, cleanup := newRepo(t)
			defer cleanup()
			w := NewWorker(cfg, r, NewEmbedder(cfg), zerolog.Nop())
			w.setMetrics(freshMetrics())
			expectDequeue(mock, ids...)
			for _, id := range ids {
				expectDLQ(mock, id, "embedding_failed", "embed http_status: HTTP 401", false)
			}
			mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, int64(len(ids))))
			w.processBatch(context.Background())
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

package memory

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// GitHub #677 (rest of #437; project RAG memory design, 2026-11-08). Found at
// review. After a SUCCESSFUL embed call, a chunk whose store failed and whose
// DLQ move then failed was acked anyway, on the batch path and on the
// per-chunk path: in neither table, never embedded, invisible to search.

func expectFailedDLQMove(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectQuery("DELETE FROM memory_embed_queue WHERE chunk_id = \\$1 RETURNING").WillReturnRows(noDLQCarry())
	mock.ExpectExec("INSERT INTO memory_embed_dlq").WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()
}

func twoVectors(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2,3]},{"index":1,"embedding":[4,5,6]}]}`))
}

func TestWorker677_BatchStoreFailureWithAFailedMoveIsNotAcked(t *testing.T) {
	cfg := Config{EmbeddingEndpoint: embedServer(t, twoVectors), EmbeddingModel: "m", EmbeddingDimension: 3}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(cfg, r, NewEmbedder(cfg), zerolog.Nop())
	w.setMetrics(freshMetrics())
	expectDequeue(mock, "c1", "c2")
	// c1: the store fails, then its DLQ move fails and rolls back.
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnError(errors.New("deadlock detected"))
	expectFailedDLQMove(mock)
	// c2: stored.
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	// The ack names c2 only: c1 keeps its claimed queue row.
	mock.ExpectExec("DELETE FROM memory_embed_queue WHERE chunk_id = ANY").
		WithArgs(ackArg{want: "c2", skip: "c1"}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w.processBatch(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorker677_PerChunkStoreFailureWithAFailedMoveIsNotAcked(t *testing.T) {
	oneVector := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2,3]}]}`))
	}
	cfg := Config{EmbeddingEndpoint: embedServer(t, oneVector), EmbeddingModel: "m", EmbeddingDimension: 3}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(cfg, r, NewEmbedder(cfg), zerolog.Nop())
	w.setMetrics(freshMetrics())
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnError(errors.New("deadlock detected"))
	expectFailedDLQMove(mock)
	// The ack the old code made. sqlmock does not fail an UNEXPECTED call (the
	// worker only logs the error it gets back), so the test plants that ack
	// and requires it to stay unmatched: the chunk reached neither outcome.
	mock.ExpectExec("DELETE FROM memory_embed_queue WHERE chunk_id = ANY").
		WithArgs(ackArg{want: "c1", skip: "\x00"}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w.processIndividually(context.Background(), []MemoryChunk{{ID: "c1", ProjectID: "p", Content: "text c1"}})
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("c1 was acked although its store and its DLQ move both failed")
	}
}

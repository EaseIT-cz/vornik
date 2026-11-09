package memory

import (
	"context"
	"database/sql/driver"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// GitHub #437 (project RAG memory design, 2026-11-08). Found at review,
// observable at production: a chunk whose DLQ move failed was acked anyway, so
// its queue row was deleted and it sat in neither the queue nor the DLQ, never
// embedded and invisible to search.

// ackArg matches the ack's pq.Array argument: it names want and not skip.
type ackArg struct{ want, skip string }

func (a ackArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, a.want) && !strings.Contains(s, a.skip)
}

func TestWorker437_AFailedDLQMoveIsNotAcked(t *testing.T) {
	url := embedServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	cfg := Config{EmbeddingEndpoint: url, EmbeddingModel: "m", EmbeddingDimension: 3}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	w := NewWorker(cfg, r, NewEmbedder(cfg), zerolog.Nop())
	w.setMetrics(freshMetrics())
	expectDequeue(mock, "c1", "c2")
	// c1: the DLQ insert fails and the move rolls back.
	mock.ExpectBegin()
	mock.ExpectQuery("DELETE FROM memory_embed_queue WHERE chunk_id = \\$1 RETURNING").WillReturnRows(noDLQCarry())
	mock.ExpectExec("INSERT INTO memory_embed_dlq").WillReturnError(errors.New("integer out of range"))
	mock.ExpectRollback()
	// c2: moved.
	expectDLQ(mock, "c2", "embedding_failed", "HTTP 401", false)
	// The ack names c2 only: c1 keeps its claimed queue row.
	mock.ExpectExec("DELETE FROM memory_embed_queue WHERE chunk_id = ANY").
		WithArgs(ackArg{want: "c2", skip: "c1"}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w.processBatch(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// dlqBackoff caps before shifting: 10m << 25 wraps int64 (review d016 F2).
func TestDLQBackoff437_CapsBeforeTheShiftWraps(t *testing.T) {
	w := &Worker{}
	for _, n := range []int{21, 25, 30, 40, 63, 64, 100} {
		if got := w.dlqBackoff(n); got != 24*time.Hour {
			t.Errorf("dlqBackoff(%d) = %v, want the 24 h cap", n, got)
		}
	}
	if got := w.dlqBackoff(1); got != 20*time.Minute {
		t.Errorf("dlqBackoff(1) = %v", got)
	}
}

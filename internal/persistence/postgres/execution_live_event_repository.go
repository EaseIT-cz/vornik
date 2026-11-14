package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// ExecutionLiveEventRepository implements
// persistence.ExecutionLiveEventRepository against PostgreSQL.
//
// Append computes the next per-execution seq via
// `SELECT COALESCE(MAX(seq) + 1, 0)` inside the INSERT. Two appends for one
// execution at once (one process publishes narration and step events
// concurrently) can read the same MAX; the (execution_id, seq) unique index
// refuses the second, and Append retries it with a fresh MAX (GitHub #662;
// the publisher's fallback does not persist, so the caller cannot be the one
// to retry). Appends for one execution are also serialised in-process
// (appendLocks): one daemon publishes an execution's events from several
// goroutines. 16 concurrent writers starved an 8-try retry in the integration
// test. The retry stays as the backstop across processes. The lock is held
// across the INSERT, which is safe because the repository is built once per
// daemon over the connection pool (storage.Build(c.instrumentedDB())), never
// over a caller's transaction; a second instance would have its own stripes.
type ExecutionLiveEventRepository struct {
	db          DBTX
	appendLocks [liveEventAppendStripes]sync.Mutex
}

// liveEventAppendStripes is how many mutexes the per-execution append lock is
// striped over; two executions share a stripe only by hash.
const liveEventAppendStripes = 64

func (r *ExecutionLiveEventRepository) appendLock(executionID string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(executionID))
	return &r.appendLocks[h.Sum32()%liveEventAppendStripes]
}

// NewExecutionLiveEventRepository constructs the repo over db.
func NewExecutionLiveEventRepository(db DBTX) *ExecutionLiveEventRepository {
	return &ExecutionLiveEventRepository{db: db}
}

// Append persists one event and returns the assigned seq.
// Payload may be nil — JSONB `null` is then stored.
func (r *ExecutionLiveEventRepository) Append(ctx context.Context, executionID, kind string, payload []byte) (int64, error) {
	if executionID == "" || kind == "" {
		return 0, fmt.Errorf("live_event: execution_id + kind required")
	}
	const q = `
INSERT INTO execution_live_events (execution_id, seq, kind, payload, created_at)
VALUES ($1, COALESCE((SELECT MAX(seq) + 1 FROM execution_live_events WHERE execution_id = $1), 0), $2, $3, NOW())
RETURNING seq`
	// Empty payload becomes JSONB null. Storing []byte(nil) makes
	// pq send the empty bytea, which JSONB rejects.
	var arg interface{} = payload
	if len(payload) == 0 {
		arg = nil
	}
	mu := r.appendLock(executionID)
	mu.Lock()
	defer mu.Unlock()
	var seq int64
	var err error
	for attempt := 1; attempt <= liveEventSeqAttempts; attempt++ {
		err = r.db.QueryRowContext(ctx, q, executionID, kind, arg).Scan(&seq)
		if err == nil {
			return seq, nil
		}
		if !isLiveEventSeqClash(err) {
			break
		}
	}
	return 0, fmt.Errorf("live_event: append: %w", err)
}

// liveEventSeqAttempts bounds Append's retries of a seq clash. Each retry
// re-reads MAX(seq), so it fails again only when yet another append won that
// race too.
const liveEventSeqAttempts = 8

// isLiveEventSeqClash reports a duplicate on idx_live_events_execution_seq,
// the only error Append retries.
func isLiveEventSeqClash(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == "idx_live_events_execution_seq"
}

// ListSince returns events with seq >= fromSeq, ordered ascending.
// limit caps the result set (0 → no cap; production callers always
// pass a sane bound).
func (r *ExecutionLiveEventRepository) ListSince(ctx context.Context, executionID string, fromSeq int64, limit int) ([]*persistence.ExecutionLiveEvent, error) {
	if executionID == "" {
		return nil, fmt.Errorf("live_event: execution_id required")
	}
	q := `
SELECT id, execution_id, seq, kind, COALESCE(payload::text, ''), created_at
FROM execution_live_events
WHERE execution_id = $1 AND seq >= $2
ORDER BY seq ASC`
	args := []interface{}{executionID, fromSeq}
	if limit > 0 {
		q += " LIMIT $3"
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("live_event: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ExecutionLiveEvent
	for rows.Next() {
		var (
			ev      persistence.ExecutionLiveEvent
			payText string
		)
		if err := rows.Scan(&ev.ID, &ev.ExecutionID, &ev.Seq, &ev.Kind, &payText, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("live_event: list scan: %w", err)
		}
		if payText != "" {
			ev.Payload = []byte(payText)
		}
		out = append(out, &ev)
	}
	return out, rows.Err()
}

// LatestSeq returns the largest seq for executionID, or -1 when no
// rows exist.
func (r *ExecutionLiveEventRepository) LatestSeq(ctx context.Context, executionID string) (int64, error) {
	if executionID == "" {
		return -1, fmt.Errorf("live_event: execution_id required")
	}
	const q = `SELECT COALESCE(MAX(seq), -1) FROM execution_live_events WHERE execution_id = $1`
	var seq int64
	if err := r.db.QueryRowContext(ctx, q, executionID).Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -1, nil
		}
		return -1, fmt.Errorf("live_event: latest_seq: %w", err)
	}
	return seq, nil
}

// DeleteOlderThan removes events with created_at older than cutoff.
// Drives the future stale-event sweeper. No cascading effects —
// events on completed executions are pure observability artifacts.
func (r *ExecutionLiveEventRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM execution_live_events WHERE created_at < $1`
	res, err := r.db.ExecContext(ctx, q, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("live_event: prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

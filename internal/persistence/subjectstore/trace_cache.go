package subjectstore

import (
	"context"
	"database/sql"
	"fmt"
)

// EvictAssembledTraces deletes every cached black-box trace, as the last step of
// an Art 17 erasure (datasubject.TraceCacheEvictor; design
// 2026-07-29-art17-redact-and-reembed §15.12, GitHub #415 item 3).
//
// Every row, not the rows of some tasks: a trace copies task_messages,
// tool_audit_log, memory_retrieval_audit and other rows that carry no subject
// link, so no query can name the traces that hold the erased data, and a
// targeted eviction would report clean about the ones it could not attribute.
// The rows are memoization; a later view re-assembles from the source rows.
//
// It also bumps the erasure generation (migration 231) in the same
// transaction, so an assembly that read pre-erasure rows cannot re-cache them
// afterwards (security review 2026-10-17, design §15.12.1).
//
// On Postgres a missing table or generation row is an error, not a silent
// zero: migrations 68 and 231 ship in both editions. A driver without the table (SQLite) reports
// applicable=false and touches nothing.
//
// see LLD § https://docs.vornik.io §15.12
func (r *Repository) EvictAssembledTraces(ctx context.Context) (int, bool, error) {
	if r == nil || r.db == nil {
		return 0, false, fmt.Errorf("subjectstore: EvictAssembledTraces requires a database handle")
	}
	if !r.d.TraceCache {
		return 0, false, nil
	}
	// One transaction: bump the erasure generation, then delete (§15.12.1). The
	// UPDATE takes the generation row's lock first, so a cache Put in flight
	// (FOR SHARE on that row) either finishes before it, and its row is then
	// seen and deleted by the DELETE below, or waits for this commit and reads
	// the new generation, which refuses its insert.
	// READ COMMITTED explicitly: each statement then takes its own snapshot, so
	// the DELETE sees a Put that committed while the UPDATE waited for it. Under
	// REPEATABLE READ the DELETE would reuse the transaction's first snapshot
	// and miss that row.
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, true, fmt.Errorf("subjectstore: begin trace-cache eviction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	bumped, err := tx.ExecContext(ctx,
		`UPDATE blackbox_trace_cache_generation SET generation = generation + 1 WHERE id = 1`)
	if err != nil {
		return 0, true, fmt.Errorf("subjectstore: bump the trace-cache erasure generation: %w", err)
	}
	if n, err := bumped.RowsAffected(); err != nil || n != 1 {
		// A missing generation row would let an in-flight Put through
		// unguarded; that is a fault (migration 231 seeds the row), not a zero.
		return 0, true, fmt.Errorf("subjectstore: the trace-cache erasure generation row is missing (rows=%d, err=%v)", n, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM blackbox_trace_cache`)
	if err != nil {
		return 0, true, fmt.Errorf("subjectstore: evict blackbox_trace_cache: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, true, fmt.Errorf("subjectstore: rows affected evicting blackbox_trace_cache: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, true, fmt.Errorf("subjectstore: commit trace-cache eviction: %w", err)
	}
	return int(n), true, nil
}

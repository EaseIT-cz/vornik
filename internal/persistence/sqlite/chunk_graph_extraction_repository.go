package sqlite

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ChunkGraphExtractionRepository drives the KG worker's chunk
// selection. SQLite uses integer (0/1) for the boolean flag.
type ChunkGraphExtractionRepository struct {
	db DBTX
}

func NewChunkGraphExtractionRepository(db DBTX) *ChunkGraphExtractionRepository {
	return &ChunkGraphExtractionRepository{db: db}
}

func (r *ChunkGraphExtractionRepository) FetchUnextracted(ctx context.Context, limit int) ([]persistence.ChunkForExtraction, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, content
		FROM project_memory_chunks
		WHERE needs_graph_extraction = 1
		  AND `+persistence.ChunkNotRestrictedSQL("project_memory_chunks.id")+`
		ORDER BY created_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]persistence.ChunkForExtraction, 0, limit)
	for rows.Next() {
		var c persistence.ChunkForExtraction
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Content); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *ChunkGraphExtractionRepository) MarkExtracted(ctx context.Context, chunkID string) error {
	if chunkID == "" {
		return fmt.Errorf("MarkExtracted: chunk_id required")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE project_memory_chunks
		 SET needs_graph_extraction = 0, graph_extraction_attempts = 0, graph_extraction_error = ''
		 WHERE id = ?`, chunkID)
	return err
}

// RecordExtractionFailure increments the failure counter and quarantines the
// chunk once it reaches maxAttempts. See the interface doc for why the bound
// exists. Single statement with RETURNING so the read-back cannot observe
// another writer's increment.
func (r *ChunkGraphExtractionRepository) RecordExtractionFailure(ctx context.Context, chunkID, reason string, maxAttempts int) (int, bool, error) {
	if chunkID == "" {
		return 0, false, fmt.Errorf("RecordExtractionFailure: chunk_id required")
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if len(reason) > persistence.ChunkExtractionErrorMaxBytes {
		reason = reason[:persistence.ChunkExtractionErrorMaxBytes]
	}
	var attempts, flagged int
	err := r.db.QueryRowContext(ctx,
		`UPDATE project_memory_chunks
		 SET graph_extraction_attempts = graph_extraction_attempts + 1,
		     graph_extraction_error    = ?,
		     needs_graph_extraction    = CASE WHEN graph_extraction_attempts + 1 < ? THEN 1 ELSE 0 END
		 WHERE id = ?
		 RETURNING graph_extraction_attempts, needs_graph_extraction`,
		reason, maxAttempts, chunkID).Scan(&attempts, &flagged)
	if err != nil {
		return 0, false, err
	}
	return attempts, flagged == 0, nil
}

func (r *ChunkGraphExtractionRepository) PendingCount(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE needs_graph_extraction = 1`,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Stats reads pending/done chunk counts + KG totals. SQLite has no
// FILTER clause; we issue separate COUNT queries.
func (r *ChunkGraphExtractionRepository) Stats(ctx context.Context) (*persistence.KGStats, error) {
	out := &persistence.KGStats{EntitiesByType: map[string]int{}}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE needs_graph_extraction = 1`,
	).Scan(&out.ChunksPending); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE needs_graph_extraction = 0`,
	).Scan(&out.ChunksDone); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_entities`).Scan(&out.Entities); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_edges`).Scan(&out.Edges); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity_mentions`).Scan(&out.Mentions); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT type, COUNT(*) FROM knowledge_entities GROUP BY type ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out.EntitiesByType[t] = n
	}
	return out, rows.Err()
}

// ReflagChunksMissingEdges — see persistence.ChunkGraphExtractionRepository.
// The SQLite path is used by single-process development deployments and
// the integration test harness; the production multi-instance deploy
// runs on postgres where source_chunks is a native TEXT[]. We mirror
// the postgres semantics using SQLite's json_each over the TEXT-stored
// source_chunks payload so the same operator gesture works on both.
//
// Schema note: the migration that created knowledge_edges in SQLite
// stores source_chunks as a JSON TEXT array (no native array type),
// so the EXISTS check has to walk it via json_each.
func (r *ChunkGraphExtractionRepository) ReflagChunksMissingEdges(ctx context.Context, projectID string, countOnly bool) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("ReflagChunksMissingEdges: project_id required")
	}
	const candidateSQL = `
		SELECT c.id
		FROM project_memory_chunks c
		WHERE c.project_id = ?
		  AND c.needs_graph_extraction = 0
		  AND NOT EXISTS (
		    SELECT 1
		    FROM knowledge_edges e, json_each(e.source_chunks) j
		    WHERE e.project_id = ?
		      AND e.lifecycle_state = 'published'
		      AND j.value = c.id
		  )`
	if countOnly {
		var n int
		err := r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ( `+candidateSQL+` )`,
			projectID, projectID).Scan(&n)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET    needs_graph_extraction = 1
		WHERE  id IN ( `+candidateSQL+` )`,
		projectID, projectID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// quarantineSelectSQLite is the quarantine predicate plus the optional
// project filter (bound twice; "" = every project). Mirrors the Postgres
// driver and idx_chunks_graph_quarantined.
const quarantineSelectSQLite = `needs_graph_extraction = 0
		  AND graph_extraction_attempts > 0
		  AND (? = '' OR project_id = ?)`

// errorMatchSQLite is the literal, case-sensitive substring match on the
// stored error (bound twice; "" = every error). instr, not LIKE: SQLite's LIKE
// is case-insensitive and reads % and _ as wildcards, unlike Postgres strpos
// (knowledge-graph LLD §3.3b).
const errorMatchSQLite = `(? = '' OR instr(graph_extraction_error, ?) > 0)`

// ReflagQuarantinedChunks — see persistence.ChunkGraphExtractionRepository.
// The census and the UPDATE share one transaction; the UPDATE re-checks the
// quarantine predicate, so a chunk that is not quarantined when it runs is
// never written.
func (r *ChunkGraphExtractionRepository) ReflagQuarantinedChunks(ctx context.Context, projectID, errorContains string, countOnly bool) ([]persistence.QuarantineReflagCount, error) {
	tx, owns, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return nil, err
	}
	x := r.db
	if owns {
		defer func() { _ = tx.Rollback() }()
		x = tx
	}
	rows, err := x.QueryContext(ctx, `
		SELECT project_id, COUNT(*), COALESCE(SUM(CASE WHEN `+errorMatchSQLite+` THEN 1 ELSE 0 END), 0)
		FROM   project_memory_chunks
		WHERE  `+quarantineSelectSQLite+`
		GROUP  BY project_id
		ORDER  BY project_id`,
		errorContains, errorContains, projectID, projectID)
	if err != nil {
		return nil, err
	}
	var census []persistence.QuarantineReflagCount
	for rows.Next() {
		var c persistence.QuarantineReflagCount
		if err := rows.Scan(&c.ProjectID, &c.Quarantined, &c.Matched); err != nil {
			_ = rows.Close()
			return nil, err
		}
		census = append(census, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if countOnly {
		return persistence.MergeQuarantineReflag(census, nil), nil
	}
	upd, err := x.QueryContext(ctx, `
		UPDATE project_memory_chunks
		SET    needs_graph_extraction    = 1,
		       graph_extraction_attempts = 0
		WHERE  `+quarantineSelectSQLite+`
		  AND  `+errorMatchSQLite+`
		RETURNING project_id`,
		projectID, projectID, errorContains, errorContains)
	if err != nil {
		return nil, err
	}
	written := map[string]int{}
	for upd.Next() {
		var p string
		if err := upd.Scan(&p); err != nil {
			_ = upd.Close()
			return nil, err
		}
		written[p]++
	}
	if err := upd.Err(); err != nil {
		_ = upd.Close()
		return nil, err
	}
	_ = upd.Close()
	if owns {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return persistence.MergeQuarantineReflag(census, written), nil
}

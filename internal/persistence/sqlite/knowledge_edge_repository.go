package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// KnowledgeEdgeRepository persists typed entity relationships.
// UpsertEdge merges on (project, from, predicate, to) — same
// invariant as the Postgres path; SQLite implementation does the
// merge in Go since SQLite lacks Postgres array_agg/unnest.
type KnowledgeEdgeRepository struct {
	db DBTX
}

func NewKnowledgeEdgeRepository(db DBTX) *KnowledgeEdgeRepository {
	return &KnowledgeEdgeRepository{db: db}
}

func (r *KnowledgeEdgeRepository) UpsertEdge(ctx context.Context, e *persistence.KnowledgeEdge) error {
	if e == nil {
		return fmt.Errorf("KnowledgeEdgeRepository.UpsertEdge: nil edge")
	}
	if e.ProjectID == "" || e.FromEntity == "" || e.ToEntity == "" || e.Predicate == "" {
		return fmt.Errorf("UpsertEdge: project_id, from, to, predicate all required")
	}
	// No self-loops (also enforced by the CHECK on knowledge_edges).
	if e.FromEntity == e.ToEntity {
		return fmt.Errorf("UpsertEdge: self-loop edge (from == to == %q) not allowed", e.FromEntity)
	}
	if len(e.SourceChunks) == 0 {
		return fmt.Errorf("UpsertEdge: source_chunks required (≥1)")
	}
	if e.ID == "" {
		e.ID = persistence.GenerateID("kedge")
	}
	if e.LifecycleState == "" {
		e.LifecycleState = "published"
	}
	if e.Confidence == 0 {
		e.Confidence = 1.0
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}

	// Look up an existing row on the merge key.
	var (
		existingID           string
		existingChunks       sqliteStringArray
		existingProps        sql.NullString
		existingConfidence   float32
		existingFaithfulness sql.NullFloat64
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT id, source_chunks, properties, confidence, faithfulness
		FROM knowledge_edges
		WHERE project_id = ? AND from_entity = ? AND predicate = ? AND to_entity = ?`,
		e.ProjectID, e.FromEntity, e.Predicate, e.ToEntity,
	).Scan(&existingID, &existingChunks, &existingProps, &existingConfidence, &existingFaithfulness)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, ierr := r.db.ExecContext(ctx, `
			INSERT INTO knowledge_edges (
				id, project_id, from_entity, to_entity, predicate,
				properties, source_chunks, extracted_by, confidence, faithfulness,
				lifecycle_state, epoch_id, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ID, e.ProjectID, e.FromEntity, e.ToEntity, e.Predicate,
			nullableBlob(e.Properties), sqliteStringArray(e.SourceChunks),
			e.ExtractedBy, e.Confidence, e.Faithfulness,
			e.LifecycleState, e.EpochID, sqliteTime(e.CreatedAt),
		)
		return ierr
	}

	// Merge path. Union source_chunks (no duplicates), max
	// confidence + faithfulness, shallow-merge JSON properties.
	mergedChunks := mergeStringSets(existingChunks, e.SourceChunks)
	newConfidence := existingConfidence
	if e.Confidence > newConfidence {
		newConfidence = e.Confidence
	}
	var newFaith *float32
	if existingFaithfulness.Valid {
		f := float32(existingFaithfulness.Float64)
		newFaith = &f
	}
	if e.Faithfulness != nil && (newFaith == nil || *e.Faithfulness > *newFaith) {
		newFaith = e.Faithfulness
	}
	var mergedProps []byte
	if existingProps.Valid {
		mergedProps = []byte(existingProps.String)
	}
	if len(e.Properties) > 0 {
		var existing, incoming map[string]any
		_ = json.Unmarshal(mergedProps, &existing)
		_ = json.Unmarshal(e.Properties, &incoming)
		if existing == nil {
			existing = map[string]any{}
		}
		for k, v := range incoming {
			existing[k] = v
		}
		buf, _ := json.Marshal(existing)
		mergedProps = buf
	}
	_, err = r.db.ExecContext(ctx, `
		UPDATE knowledge_edges
		SET source_chunks = ?,
		    properties    = ?,
		    confidence    = ?,
		    faithfulness  = ?
		WHERE id = ?`,
		sqliteStringArray(mergedChunks), nullableBlob(mergedProps),
		newConfidence, newFaith, existingID,
	)
	return err
}

func mergeStringSets(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		seen[s] = struct{}{}
	}
	for _, s := range b {
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for _, s := range a {
		if _, ok := seen[s]; ok {
			out = append(out, s)
			delete(seen, s)
		}
	}
	for _, s := range b {
		if _, ok := seen[s]; ok {
			out = append(out, s)
			delete(seen, s)
		}
	}
	return out
}

func (r *KnowledgeEdgeRepository) Get(ctx context.Context, id string) (*persistence.KnowledgeEdge, error) {
	if id == "" {
		return nil, fmt.Errorf("KnowledgeEdgeRepository.Get: id required")
	}
	row := r.db.QueryRowContext(ctx, knowledgeEdgeSelect+` WHERE id = ?`, id)
	return scanKnowledgeEdge(row)
}

func (r *KnowledgeEdgeRepository) List(ctx context.Context, filter persistence.KnowledgeEdgeFilter) ([]*persistence.KnowledgeEdge, error) {
	if filter.ProjectID == "" {
		return nil, fmt.Errorf("KnowledgeEdgeRepository.List: project_id required")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	lifecycle := filter.Lifecycle
	if len(lifecycle) == 0 {
		lifecycle = []string{"published"}
	}
	var b strings.Builder
	b.WriteString(knowledgeEdgeSelect)
	b.WriteString(" WHERE project_id = ?")
	args := []any{filter.ProjectID}

	lp := strings.Repeat("?,", len(lifecycle))
	lp = lp[:len(lp)-1]
	b.WriteString(" AND lifecycle_state IN (" + lp + ")")
	for _, l := range lifecycle {
		args = append(args, l)
	}
	if filter.FromEntity != "" {
		b.WriteString(" AND from_entity = ?")
		args = append(args, filter.FromEntity)
	}
	if filter.ToEntity != "" {
		b.WriteString(" AND to_entity = ?")
		args = append(args, filter.ToEntity)
	}
	if filter.Predicate != "" {
		b.WriteString(" AND predicate = ?")
		args = append(args, filter.Predicate)
	}
	b.WriteString(" ORDER BY created_at DESC LIMIT ?")
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.KnowledgeEdge, 0)
	for rows.Next() {
		e, err := scanKnowledgeEdge(rows)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

func (r *KnowledgeEdgeRepository) EdgesForEntity(ctx context.Context, entityID string, limit int) ([]*persistence.KnowledgeEdge, error) {
	if entityID == "" {
		return nil, fmt.Errorf("EdgesForEntity: entityID required")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, knowledgeEdgeSelect+`
		WHERE (from_entity = ? OR to_entity = ?)
		  AND lifecycle_state = 'published'
		ORDER BY created_at DESC
		LIMIT ?`, entityID, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.KnowledgeEdge, 0, limit)
	for rows.Next() {
		e, err := scanKnowledgeEdge(rows)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

func (r *KnowledgeEdgeRepository) UpdateLifecycle(ctx context.Context, id, newState string) error {
	if id == "" || newState == "" {
		return fmt.Errorf("UpdateLifecycle: id + newState required")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE knowledge_edges SET lifecycle_state = ? WHERE id = ?`,
		newState, id)
	return err
}

// DropChunkFromSources — see persistence.KnowledgeEdgeRepository. One UPDATE,
// the same shape as the Postgres driver (GitHub #450; KG design §5.1
// amendment 2026-10-21): json_each does what array_remove / ANY do there.
// Exact membership, not LIKE (which read _ and % in an id as wildcards);
// json_group_array keeps json_each order, so survivors keep their order.
// RowsAffected is the number of edges whose source_chunks changed.
func (r *KnowledgeEdgeRepository) DropChunkFromSources(ctx context.Context, chunkID string) (int, error) {
	if chunkID == "" {
		return 0, fmt.Errorf("DropChunkFromSources: chunk_id required")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE knowledge_edges
		SET source_chunks = (SELECT json_group_array(j.value)
		                       FROM json_each(knowledge_edges.source_chunks) AS j
		                      WHERE j.value <> ?1),
		    lifecycle_state = CASE
		        WHEN EXISTS (SELECT 1 FROM json_each(knowledge_edges.source_chunks) AS j
		                      WHERE j.value <> ?1)
		        THEN lifecycle_state ELSE 'quarantined' END
		WHERE EXISTS (SELECT 1 FROM json_each(knowledge_edges.source_chunks) AS j
		               WHERE j.value = ?1)`,
		chunkID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

const knowledgeEdgeSelect = `
SELECT id, project_id, from_entity, to_entity, predicate,
       properties, source_chunks, extracted_by, confidence, faithfulness,
       lifecycle_state, epoch_id, created_at
FROM knowledge_edges`

func scanKnowledgeEdge(scanner interface{ Scan(dest ...any) error }) (*persistence.KnowledgeEdge, error) {
	var (
		e            persistence.KnowledgeEdge
		properties   sql.NullString
		sourceChunks sqliteStringArray
		extractedBy  sql.NullString
		faithfulness sql.NullFloat64
		epochID      sql.NullString
		createdAt    sqlTime
	)
	err := scanner.Scan(
		&e.ID, &e.ProjectID, &e.FromEntity, &e.ToEntity, &e.Predicate,
		&properties, &sourceChunks, &extractedBy, &e.Confidence, &faithfulness,
		&e.LifecycleState, &epochID, &createdAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if properties.Valid {
		e.Properties = []byte(properties.String)
	}
	e.SourceChunks = []string(sourceChunks)
	if extractedBy.Valid {
		e.ExtractedBy = extractedBy.String
	}
	if faithfulness.Valid {
		f := float32(faithfulness.Float64)
		e.Faithfulness = &f
	}
	if epochID.Valid {
		e.EpochID = &epochID.String
	}
	e.CreatedAt = createdAt.Time
	return &e, nil
}

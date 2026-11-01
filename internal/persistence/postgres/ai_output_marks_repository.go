package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// AIOutputMarkRepository is the Postgres store of the Art 50(2) marks and
// signing keys (migration 243; GitHub #117).
type AIOutputMarkRepository struct {
	db persistence.DBTX
}

// NewAIOutputMarkRepository builds the repository.
func NewAIOutputMarkRepository(db persistence.DBTX) *AIOutputMarkRepository {
	return &AIOutputMarkRepository{db: db}
}

var _ persistence.AIOutputMarkRepository = (*AIOutputMarkRepository)(nil)

// Record writes a mark; the first write for a hash wins.
func (r *AIOutputMarkRepository) Record(ctx context.Context, m persistence.AIOutputMark) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO ai_output_marks (content_sha256, surface, mark, task_id, issued_at)
		      VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		 ON CONFLICT (content_sha256) DO NOTHING`,
		m.ContentSHA256, m.Surface, m.Mark, m.TaskID, m.IssuedAt.UTC())
	if err != nil {
		return fmt.Errorf("postgres: ai_output_marks insert: %w", err)
	}
	return nil
}

// Get returns the mark for a content hash.
func (r *AIOutputMarkRepository) Get(ctx context.Context, contentSHA256 string) (*persistence.AIOutputMark, error) {
	var (
		m      persistence.AIOutputMark
		taskID sql.NullString
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT content_sha256, surface, mark, task_id, issued_at
		   FROM ai_output_marks WHERE content_sha256 = $1`, contentSHA256,
	).Scan(&m.ContentSHA256, &m.Surface, &m.Mark, &taskID, &m.IssuedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: ai_output_marks get: %w", err)
	}
	m.TaskID = taskID.String
	m.IssuedAt = m.IssuedAt.UTC()
	return &m, nil
}

// RecordKey writes a signing key; the first write for a kid wins.
func (r *AIOutputMarkRepository) RecordKey(ctx context.Context, k persistence.AIMarkKey) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO ai_mark_keys (kid, public_key, first_used_at)
		      VALUES ($1, $2, $3)
		 ON CONFLICT (kid) DO NOTHING`,
		k.KID, k.PublicKey, k.FirstUsedAt.UTC())
	if err != nil {
		return fmt.Errorf("postgres: ai_mark_keys insert: %w", err)
	}
	return nil
}

// Keys returns every signing key, oldest first.
func (r *AIOutputMarkRepository) Keys(ctx context.Context) ([]persistence.AIMarkKey, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kid, public_key, first_used_at FROM ai_mark_keys ORDER BY first_used_at, kid`)
	if err != nil {
		return nil, fmt.Errorf("postgres: ai_mark_keys list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AIMarkKey
	for rows.Next() {
		var k persistence.AIMarkKey
		if err := rows.Scan(&k.KID, &k.PublicKey, &k.FirstUsedAt); err != nil {
			return nil, fmt.Errorf("postgres: ai_mark_keys scan: %w", err)
		}
		k.FirstUsedAt = k.FirstUsedAt.UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}

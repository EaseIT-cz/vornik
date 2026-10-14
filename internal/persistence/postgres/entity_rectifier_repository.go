package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// GDPR increment 6 (GitHub #343), Art 16: the store half of correcting a
// knowledge-graph entity's name, aliases and description.
//
// Kept OUT of KnowledgeEntityRepository, for the reason §4.14 kept graph hard
// deletes out of it: every other caller of that repository has no business
// rewriting an entity's identity, and a method there would be one autocomplete
// away from all of them. This type is reachable only from `subject rectify`.
//
// see LLD § https://docs.vornik.io §4.16.2

// EntityRectifierRepository implements datasubject.EntityRectifier.
type EntityRectifierRepository struct {
	db *sql.DB
}

// NewEntityRectifierRepository wires the store; nil for a nil handle.
func NewEntityRectifierRepository(db *sql.DB) *EntityRectifierRepository {
	if db == nil {
		return nil
	}
	return &EntityRectifierRepository{db: db}
}

var _ datasubject.EntityRectifier = (*EntityRectifierRepository)(nil)

// LoadEntity reads the entity's personal-data-bearing text.
func (r *EntityRectifierRepository) LoadEntity(ctx context.Context, entityID string) (datasubject.EntityText, error) {
	var (
		t       datasubject.EntityText
		aliases []byte
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT canonical_name, aliases, description FROM knowledge_entities WHERE id = $1`, entityID,
	).Scan(&t.CanonicalName, &aliases, &t.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("postgres: knowledge entity %q not found", entityID)
	}
	if err != nil {
		return t, fmt.Errorf("postgres: load knowledge entity %q: %w", entityID, err)
	}
	if len(aliases) > 0 {
		if err := json.Unmarshal(aliases, &t.Aliases); err != nil {
			return t, fmt.Errorf("postgres: decode aliases of %q: %w", entityID, err)
		}
	}
	return t, nil
}

// RectifyEntity writes updated, guarded on the row still holding expected.
//
// One statement: the corrected text AND `embedding = NULL`, because the vector was
// computed from the inaccurate canonical name and nothing recomputes it — keeping
// it would leave the inaccuracy reachable by similarity search. A collision with
// another entity's (project_id, type, canonical_name) is reported as a conflict,
// never resolved by merging.
func (r *EntityRectifierRepository) RectifyEntity(
	ctx context.Context, entityID string, expected, updated datasubject.EntityText,
) (datasubject.EntityRectifyOutcome, error) {
	expAliases, err := marshalAliases(expected.Aliases)
	if err != nil {
		return "", err
	}
	newAliases, err := marshalAliases(updated.Aliases)
	if err != nil {
		return "", err
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE knowledge_entities
		   SET canonical_name = $5, aliases = $6::jsonb, description = $7,
		       embedding = NULL, updated_at = NOW()
		 WHERE id = $1 AND canonical_name = $2 AND aliases = $3::jsonb AND description = $4`,
		entityID, expected.CanonicalName, expAliases, expected.Description,
		updated.CanonicalName, newAliases, updated.Description)
	if err = mapDBError(err); err != nil {
		if errors.Is(err, persistence.ErrDuplicateKey) {
			return datasubject.EntityRectifyConflict, nil
		}
		return "", fmt.Errorf("postgres: rectify knowledge entity %s: %w", entityID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("postgres: rows affected rectifying %s: %w", entityID, err)
	}
	if n == 0 {
		return datasubject.EntityRectifyVersionChanged, nil
	}
	return datasubject.EntityRectifyApplied, nil
}

func marshalAliases(a []string) (string, error) {
	if a == nil {
		a = []string{}
	}
	b, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("postgres: encode aliases: %w", err)
	}
	return string(b), nil
}

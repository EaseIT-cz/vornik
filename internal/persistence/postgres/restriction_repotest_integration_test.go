//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// audit 2026-10-12 (security review of #343): graph extraction fetched chunks
// under an Art 18 restriction on both drivers. Same suite as the SQLite file.
func TestChunkGraphExtraction_RestrictionPostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunChunkGraphExtractionRestrictionSuite(t, NewChunkGraphExtractionRepository(db.DB),
		seedChunkForSuite(db), restrictChunkForSuite(db))
}

type seedStmt struct {
	q    string
	args []any
}

// restrictChunkForSuite seeds a subject, a verified restriction request, the link
// (direct, or to an entity the chunk mentions) and an active restriction — the
// rows the shared predicate reads, satisfying the Postgres foreign keys.
func restrictChunkForSuite(db *DB) repotest.RestrictChunk {
	return func(ctx context.Context, chunkID, projectID string, viaEntity bool) (func() error, error) {
		subject, req, rid := "ds_"+chunkID, "dsr_"+chunkID, "dsx_"+chunkID
		stmts := []seedStmt{
			{`INSERT INTO data_subjects (id, display_name) VALUES ($1, 'suite')`, []any{subject}},
			{`INSERT INTO data_subject_requests (id, subject_id, kind, state, opened_at)
			  VALUES ($1, $2, 'restriction', 'verified', NOW())`, []any{req, subject}},
		}
		table, row := "project_memory_chunks", chunkID
		if viaEntity {
			ent := "ent_" + chunkID
			stmts = append(stmts,
				seedStmt{`INSERT INTO knowledge_entities (id, project_id, type, canonical_name)
				          VALUES ($1, $2, 'PERSON', $3)`, []any{ent, projectID, "Person " + chunkID}},
				seedStmt{`INSERT INTO entity_mentions (chunk_id, entity_id, char_start) VALUES ($1, $2, 0)`,
					[]any{chunkID, ent}})
			table, row = "knowledge_entities", ent
		}
		stmts = append(stmts,
			seedStmt{`INSERT INTO data_subject_links (subject_id, table_name, row_id, project_id, source, confidence, exclusivity)
			          VALUES ($1, $2, $3, $4, 'operator_asserted', 'probable', 'exclusive')`,
				[]any{subject, table, row, projectID}},
			seedStmt{`INSERT INTO data_subject_restrictions (id, subject_id, request_id, ground, restricted_by)
			          VALUES ($1, $2, $3, 'art18_1_a', 'suite')`, []any{rid, subject, req}})
		for _, s := range stmts {
			if _, err := db.DB.ExecContext(ctx, s.q, s.args...); err != nil {
				return nil, err
			}
		}
		return func() error {
			_, err := db.DB.ExecContext(context.Background(), `UPDATE data_subject_restrictions
				SET lifted_at = NOW(), lifted_by = 'suite', lift_reason = 'test', subject_informed_at = NOW()
				WHERE id = $1`, rid)
			return err
		}, nil
	}
}

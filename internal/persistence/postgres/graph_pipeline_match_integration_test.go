//go:build integration

package postgres

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #419 (found in production): 402 chunks quarantined with
// `insert mention for entity ... not found` and 13 with `upsert edge ...: not
// found` — a foreign-key violation on a resolver match_id that named no
// entity. Postgres is the driver that refuses the id, so this lane reproduces
// the production failure. Same suite as the SQLite file.
func TestGraphPipelineResolverMatch_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunGraphPipelineResolverMatchSuite(t, repotest.GraphPipelineRepos{
		Entities: NewKnowledgeEntityRepository(db.DB),
		Edges:    NewKnowledgeEdgeRepository(db.DB),
		Mentions: NewEntityMentionRepository(db.DB),
	}, seedChunkForSuite(db))
}

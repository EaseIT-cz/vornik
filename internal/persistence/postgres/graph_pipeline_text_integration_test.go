//go:build integration

package postgres

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
)

// GitHub #284 (found in production): 1,760 chunks quarantined with
// `insert mention ...: pq: invalid byte sequence for encoding "UTF8"` — the
// mention surface was sliced at model offsets inside a multi-byte character.
// Postgres is the driver that refuses the bytes, so this is the lane that
// reproduces the production failure. Same suite as the SQLite file.
func TestGraphPipelineStorableText_PostgresContract(t *testing.T) {
	db := newIntegrationDB(t)
	repotest.RunGraphPipelineStorableTextSuite(t, repotest.GraphPipelineRepos{
		Entities: NewKnowledgeEntityRepository(db.DB),
		Edges:    NewKnowledgeEdgeRepository(db.DB),
		Mentions: NewEntityMentionRepository(db.DB),
	}, seedChunkForSuite(db))
}

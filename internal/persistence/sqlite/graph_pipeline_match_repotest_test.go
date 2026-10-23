package sqlite_test

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #419: a resolver match_id naming no entity of the project. SQLite
// (foreign keys off) stored the mention and the edge pointing at nothing
// where Postgres failed the chunk; same suite as the Postgres lane.
func TestGraphPipelineResolverMatch_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunGraphPipelineResolverMatchSuite(t, repotest.GraphPipelineRepos{
		Entities: sqlite.NewKnowledgeEntityRepository(db.DB),
		Edges:    sqlite.NewKnowledgeEdgeRepository(db.DB),
		Mentions: sqlite.NewEntityMentionRepository(db.DB),
	}, seedChunkForSuite(db))
}

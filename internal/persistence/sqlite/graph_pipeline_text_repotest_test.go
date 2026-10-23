package sqlite_test

import (
	"testing"

	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// GitHub #284: the KG pipeline persisted a mention surface sliced at model
// offsets inside a multi-byte character. SQLite stored the broken bytes where
// Postgres failed the chunk; same suite as the Postgres lane.
func TestGraphPipelineStorableText_Contract(t *testing.T) {
	db := newTestDB(t)
	repotest.RunGraphPipelineStorableTextSuite(t, repotest.GraphPipelineRepos{
		Entities: sqlite.NewKnowledgeEntityRepository(db.DB),
		Edges:    sqlite.NewKnowledgeEdgeRepository(db.DB),
		Mentions: sqlite.NewEntityMentionRepository(db.DB),
	}, seedChunkForSuite(db))
}

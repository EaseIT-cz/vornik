package service

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/memory"
)

// The test seam the pgvector lane uses (test/integration,
// companion_recall_scope_test.go, GitHub #368) must be the production
// constructor, not a second one: it refuses a half-wired adapter exactly as
// newMemoryCompanionAdapter does, and returns the production adapter type.
func TestNewMemoryCompanionAdapterForTesting_IsTheProductionConstructor(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	s := memory.NewSearcher(cfg, repo, nil)
	p := memory.NewPipeline(memory.NewIndexer(cfg, repo, nil, zerolog.Nop()), memory.PipelineConfig{Logger: zerolog.Nop()})

	require.Nil(t, NewMemoryCompanionAdapterForTesting(nil, p, repo, 0), "half-wired: no searcher")
	a := NewMemoryCompanionAdapterForTesting(s, p, repo, 0.2)
	got, ok := a.(*memoryCompanionAdapter)
	require.True(t, ok, "the seam must return the production adapter")
	require.Equal(t, 0.2, got.minRefuteScore)
}

package service

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memory"
)

// GitHub #436 part 2 (design 22, "Amendment 2026-10-21 (#436)"): a companion
// correction is stored as a companion deposit, which needs a client kind and a
// key id. A call that cannot store its correction is refused before the
// refute, so it is never half-applied. The behavioural test is on the
// pgvector lane (TestIntegration_CompanionMemoryCorrectionPassesTheCompanionGates).
func TestMemoryCompanionAdapter_CorrectionWithoutClientKindRefusedBeforeRefute(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	s := memory.NewSearcher(cfg, repo, nil)
	p := memory.NewPipeline(memory.NewIndexer(cfg, repo, nil, zerolog.Nop()), memory.PipelineConfig{Logger: zerolog.Nop()})
	a := NewMemoryCompanionAdapterForTesting(s, p, repo, 0)

	_, err = a.Correct(context.Background(), api.CorrectInput{
		ProjectID: "p", WrongClaim: "claim", Correction: "the fix", ActorID: "key-1",
	})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet(), "no SQL may run before the refusal")
}

// The other two disjuncts of the refusal (review ee12 F4).
func TestMemoryCompanionAdapter_CorrectionRefusedWithoutPipelineOrKeyID(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	s := memory.NewSearcher(cfg, repo, nil)
	p := memory.NewPipeline(memory.NewIndexer(cfg, repo, nil, zerolog.Nop()), memory.PipelineConfig{Logger: zerolog.Nop()})

	noKey := NewMemoryCompanionAdapterForTesting(s, p, repo, 0)
	_, err = noKey.Correct(context.Background(), api.CorrectInput{ProjectID: "p", WrongClaim: "claim", Correction: "fix", ClientKind: "claude-code"})
	require.Error(t, err, "a correction without a key id must be refused")

	noPipeline := &memoryCompanionAdapter{s: s, repo: repo}
	_, err = noPipeline.Correct(context.Background(), api.CorrectInput{ProjectID: "p", WrongClaim: "claim", Correction: "fix", ClientKind: "claude-code", ActorID: "key-1"})
	require.Error(t, err, "a correction without a pipeline must be refused")
	require.NoError(t, mock.ExpectationsWereMet(), "no SQL may run before the refusal")
}

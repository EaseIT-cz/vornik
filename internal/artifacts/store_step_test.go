package artifacts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #90: successive step revisions must preserve their own bytes.
func TestStore_ProducingStepAndImmutableRevision(t *testing.T) {
	s, _, _ := rtNewStore(t)
	ctx := persistence.WithArtifactStep(context.Background(), "answer")
	a, err := s.Store(ctx, "p1", "e1", "t1", "result.json", rtWriteSource(t, "a.json", []byte("first")))
	require.NoError(t, err)
	require.Equal(t, "answer", a.ProducingStepID)
	b, err := s.Store(ctx, "p1", "e1", "t1", "result.json", rtWriteSource(t, "b.json", []byte("second")))
	require.NoError(t, err)
	require.NotEqual(t, a.StoragePath, b.StoragePath)
	body, err := s.Retrieve(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "first", string(body))
	body, err = s.Retrieve(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "second", string(body))
}

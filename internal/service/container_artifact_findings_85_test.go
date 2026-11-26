package service

// GitHub #85 (artifact heuristic-redaction design §7): the findings counter is
// wired once, on the registry /metrics serves. Pass 1 of initHTTPServer has no
// registry, and a second registration would panic in promauto.
//
// Found at: review (review-20261119-54ae F1, 2026-11-20), observed at unit.

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/artifacts"
)

func TestWireArtifactFindings85_OnceOnTheServedRegistry(t *testing.T) {
	store, err := artifacts.New(artifacts.WithBasePath(t.TempDir()), artifacts.WithLogger(zerolog.Nop()))
	require.NoError(t, err)
	c := &Container{artifactStore: store}

	c.wireArtifactFindings(nil) // pass 1: no registry yet
	assert.Nil(t, c.artifactFindings, "pass 1 wires nothing")

	reg := prometheus.NewRegistry()
	c.wireArtifactFindings(reg) // pass 2
	require.NotNil(t, c.artifactFindings)
	first := c.artifactFindings
	assert.NotPanics(t, func() { c.wireArtifactFindings(reg) }, "a second call must not register twice")
	assert.Equal(t, first, c.artifactFindings, "the same counter, not a second one")

	c.artifactFindings.Inc("entropy", artifacts.FindingDropped)
	n, err := testutil.GatherAndCount(reg, "vornik_artifact_secret_findings_total")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the counter is on the registry it was given")
}

func TestWireArtifactFindings85_NoStoreNoCounter(t *testing.T) {
	c := &Container{}
	c.wireArtifactFindings(prometheus.NewRegistry())
	assert.Nil(t, c.artifactFindings)
}

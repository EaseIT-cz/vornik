package service

import (
	"github.com/prometheus/client_golang/prometheus"

	"vornik.io/vornik/internal/artifacts"
)

// wireArtifactFindings gives the artifact store its findings counter on reg,
// the registry /metrics serves (GitHub #85, artifact heuristic-redaction
// design §7). initHTTPServer calls it only on the pass where that registry
// exists; the field guard keeps a second call from registering the counter
// twice, which promauto refuses with a panic.
func (c *Container) wireArtifactFindings(reg prometheus.Registerer) {
	if reg == nil || c.artifactFindings != nil || c.artifactStore == nil {
		return
	}
	c.artifactFindings = artifacts.NewPromFindingsCounter(reg)
	c.artifactStore.SetFindingsCounter(c.artifactFindings)
}

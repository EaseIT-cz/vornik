package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/egressscan"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// egressScanner builds the outbound secret scan once (agent-administered
// Vornik plan P5.1; Part A of the 2026-07-16 secret-egress design). The
// detector is built from the secrets config even when secrets.enabled is
// off: that switch governs what is stored, and agent projects must not lose
// their egress scan to it. A detector that fails to build leaves the scan
// nil, which refuses every agent-project tool call (fail closed) and passes
// operator ones (Part A: operator scans fail open).
func (c *Container) egressScanner() *api.EgressScan {
	c.egressOnce.Do(func() {
		if c.Config == nil {
			return
		}
		d, actions, err := buildSecretsDetector(c.Config.Secrets)
		if err != nil || d == nil {
			c.Logger.Error().Err(err).Msg("egress secret scan unavailable: agent-project tool calls will be refused")
			return
		}
		if c.egressMetrics == nil {
			c.egressMetrics = egressscan.NewMetrics()
		}
		c.egressScan = &api.EgressScan{
			Detector: d,
			Policy: func(string) secrets.Action {
				if !c.Config.Secrets.Enabled {
					return secrets.ActionDetect
				}
				return secrets.ResolveAction(secrets.CheckpointToolEgress, actions)
			},
			Record: c.recordEgress,
		}
	})
	return c.egressScan
}

// recordEgress counts an examined document and, for findings, logs their
// types and paths and writes redaction-audit rows - never a value.
func (c *Container) recordEgress(surface, projectID, what string, fs []egressscan.Finding, action secrets.Action) {
	c.egressMetrics.Observe(surface, projectID, fs, string(action))
	if len(fs) == 0 {
		return
	}
	counts := map[string]int{}
	var where []string
	for _, f := range fs {
		counts[f.Type]++
		where = append(where, f.Type+"@"+f.Path)
	}
	sort.Strings(where)
	c.Logger.Warn().Str("surface", surface).Str("project", projectID).Str("tool", what).
		Str("action", string(action)).Str("findings", strings.Join(where, ", ")).
		Msg("egress secret scan: credential-shaped values in outbound data")
	if c.repos == nil || c.repos.SecretRedaction == nil {
		return
	}
	events := make([]persistence.SecretRedactionEvent, 0, len(counts))
	for t, n := range counts {
		events = append(events, persistence.SecretRedactionEvent{ProjectID: projectID,
			Checkpoint: secrets.CheckpointToolEgress, FindingType: t, Count: n, Source: "live"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.repos.SecretRedaction.Record(ctx, events); err != nil {
		c.Logger.Warn().Err(err).Str("surface", surface).Msg("egress secret scan: audit rows not written")
	}
}

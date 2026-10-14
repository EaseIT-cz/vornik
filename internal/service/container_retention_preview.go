package service

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/retention"
	"vornik.io/vornik/internal/ui"
)

// retentionPreviewAdapter satisfies ui.RetentionPreviewer. Wraps
// retention.Sweeper.Preview + builds the per-project Policy by
// merging daemon defaults with the project's YAML overrides.
//
// Constructed at HTTP wire-up time. The sweeper itself is
// stateless (db handle + logger only), so this is safe alongside
// the background sweeper goroutine started in container_autonomy.go.
type retentionPreviewAdapter struct {
	sweeper       *retention.Sweeper
	defaults      config.RetentionConfig
	artifactsRoot string
	registry      *registry.Registry
}

func newRetentionPreviewAdapter(sweeper *retention.Sweeper, cfg config.RetentionConfig, artifactsRoot string, reg *registry.Registry) ui.RetentionPreviewer {
	if sweeper == nil {
		return nil
	}
	return &retentionPreviewAdapter{
		sweeper:       sweeper,
		defaults:      cfg,
		artifactsRoot: artifactsRoot,
		registry:      reg,
	}
}

func (a *retentionPreviewAdapter) Preview(ctx context.Context, projectID string) (ui.RetentionPreviewCounts, error) {
	if a == nil || a.sweeper == nil {
		return ui.RetentionPreviewCounts{}, fmt.Errorf("retention preview not configured")
	}
	if projectID == "" {
		return ui.RetentionPreviewCounts{}, fmt.Errorf("project id required")
	}
	policy := a.resolvePolicy(projectID)
	counts, err := a.sweeper.Preview(ctx, policy)
	if err != nil {
		return ui.RetentionPreviewCounts{}, err
	}
	return ui.RetentionPreviewCounts{
		TaskLLMUsage:  counts.TaskLLMUsage,
		ToolAudit:     counts.ToolAudit,
		Tasks:         counts.Tasks,
		Executions:    counts.Executions,
		Artifacts:     counts.Artifacts,
		ArtifactFiles: counts.ArtifactFiles,
		TaskMessages:  counts.TaskMessages,
		MemoryChunks:  counts.MemoryChunks,
	}, nil
}

// resolvePolicy merges the daemon-wide retention defaults with the
// project's YAML overrides through retention.Resolve — the same function the
// background sweeper and `vornikctl retention` use — so the preview shows
// exactly what the next sweep would do: compiled defaults for unset windows,
// the minimum floors, and the tool_audit_log six-month floor.
//
// audit 2026-10-12: this used to merge by hand and applied none of those, so
// an unset window previewed as a threshold of "now" and a sub-floor
// tool_audit_days previewed deletions the sweep would not make.
func (a *retentionPreviewAdapter) resolvePolicy(projectID string) retention.Policy {
	d := a.defaults
	floor, _ := retention.ResolveToolAuditFloor(d.ToolAuditFloorDays, d.ToolAuditFloorOverride, d.ToolAuditFloorOverrideBasis)
	defaults := retention.DefaultsFromConfig(d, floor, a.artifactsRoot)
	var per retention.Policy
	if a.registry != nil {
		if p := a.registry.GetProject(projectID); p != nil {
			per = retention.ProjectOverrides(p.Retention)
		}
	}
	return retention.Resolve(projectID, per, defaults)
}

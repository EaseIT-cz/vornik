package retention

import (
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

// The one mapping from configuration to a Policy, shared by the daemon's
// sweeper, its preview endpoint and `vornikctl retention` (#334 audit
// 2026-10-14). Each of the three used to copy the fields by hand, and the
// CLI's copy had lost MemoryEvictionAuditDays, so `vornikctl retention
// --apply` swept eviction tombstones on the compiled 365-day default while
// the daemon kept them for the configured window.
// TestPolicyFromConfig_CopiesEveryWindow fails when a window field exists on
// both sides and is not copied here.

// DefaultsFromConfig is the daemon-wide default Policy from the
// `retention:` block. toolAuditFloorDays is the floor the caller resolved
// with ResolveToolAuditFloor (the caller owns logging its warning).
func DefaultsFromConfig(rc config.RetentionConfig, toolAuditFloorDays int, artifactsRoot string) Policy {
	return Policy{
		TaskLLMUsageDays:          rc.TaskLLMUsageDays,
		ToolAuditDays:             rc.ToolAuditDays,
		ToolAuditFloorDays:        toolAuditFloorDays,
		ChatAuditDays:             rc.ChatAuditDays,
		TasksDays:                 rc.TasksDays,
		ExecutionsDays:            rc.ExecutionsDays,
		ArtifactsDays:             rc.ArtifactsDays,
		TaskMessagesDays:          rc.TaskMessagesDays,
		MemoryChunksDays:          rc.MemoryChunksDays,
		GraphQuarantineDays:       rc.GraphQuarantineDays,
		MemoryIngestAuditDays:     rc.MemoryIngestAuditDays,
		MemoryPolicyEvalAllowDays: rc.MemoryPolicyEvalAllowDays,
		MemoryPolicyEvalBlockDays: rc.MemoryPolicyEvalBlockDays,
		MemoryEvictionAuditDays:   rc.MemoryEvictionAuditDays,
		ArtifactsRoot:             artifactsRoot,
	}
}

// ProjectOverrides is a project's `retention:` block as the per-project
// half of Resolve.
func ProjectOverrides(pr registry.ProjectRetention) Policy {
	return Policy{
		MemoryRequireApproval:     pr.MemoryRequireApproval,
		TaskLLMUsageDays:          pr.TaskLLMUsageDays,
		ToolAuditDays:             pr.ToolAuditDays,
		ChatAuditDays:             pr.ChatAuditDays,
		TasksDays:                 pr.TasksDays,
		ExecutionsDays:            pr.ExecutionsDays,
		ArtifactsDays:             pr.ArtifactsDays,
		TaskMessagesDays:          pr.TaskMessagesDays,
		MemoryChunksDays:          pr.MemoryChunksDays,
		MemoryIngestAuditDays:     pr.MemoryIngestAuditDays,
		MemoryPolicyEvalAllowDays: pr.MemoryPolicyEvalAllowDays,
		MemoryPolicyEvalBlockDays: pr.MemoryPolicyEvalBlockDays,
		MemoryEvictionAuditDays:   pr.MemoryEvictionAuditDays,
	}
}

// GlobalFromConfig is the one mapping from the `retention:` block to the
// global sweep's horizons and the upgrade-dump policy (GitHub #394, design
// 2026-10-12-tool-audit-retention-floor §8.2.4), shared by the daemon
// sweeper, `vornikctl retention upgrade-dumps` and the upgrade_dumps doctor
// check. ConfigApplyJournalDays has no config key and stays at its default.
// TestGlobalFromConfig_CopiesEveryKey guards it.
func GlobalFromConfig(rc config.RetentionConfig) (GlobalPolicy, UpgradeDumpPolicy) {
	return GlobalPolicy{
			ResponseCacheDays:      rc.ResponseCacheDays,
			EmbeddingCacheDays:     rc.EmbeddingCacheDays,
			ExecutionRatingsDays:   rc.ExecutionRatingsDays,
			BlackboxTraceCacheDays: rc.BlackboxTraceCacheDays,
			ProjectGistsDays:       rc.ProjectGistsDays,
		}, UpgradeDumpPolicy{
			Root:      rc.UpgradeDumpDir,
			Keep:      rc.UpgradeDumpKeep,
			GraceDays: rc.UpgradeDumpGraceDays,
		}
}

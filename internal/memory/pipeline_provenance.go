package memory

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// The role-of-record decision for a companion-delegated task (GitHub #482,
// design 22 "Amendment 2026-10-21 (#482): a companion-delegated chunk is
// verified only by provenance").

// companionDelegated asks the wired hook whether the task is companion-
// delegated. A lookup error counts as delegated: an ingest whose origin
// cannot be read never takes a role's authority. A companion or chat producer
// is already capped by EffectiveClassPolicy and is not asked.
func (p *Pipeline) companionDelegated(ctx context.Context, projectID, taskID, producerRole string) bool {
	if isDeliberateHumanDeposit(producerRole) {
		// Exclusive with the gate: EffectiveClassPolicy never makes a
		// companion or chat producer role-of-record, so it never reaches
		// here as eligible (pinned by TestHumanDepositNeverEligible).
		return false
	}
	if p.cfg.CompanionDelegated == nil {
		// Unwired: the origin cannot be read, so the task counts as
		// delegated (round-3 review B1). The daemon always wires it.
		p.recordProvenanceOutcome(projectID, "origin_unknown")
		return true
	}
	delegated, err := p.cfg.CompanionDelegated(ctx, taskID)
	if err != nil {
		p.logger.Warn().Err(err).Str("project_id", projectID).Str("task_id", taskID).
			Msg("pipeline: task origin unreadable; treated as companion-delegated, not role-of-record (fail closed)")
		p.recordProvenanceOutcome(projectID, "origin_unknown")
		return true
	}
	return delegated
}

// verifyProvenance runs the provenance rule for a companion-delegated ingest
// and stamps the chunks verified only when it holds. The record is written
// first, so there is never a stamp without a record; a failed stamp after a
// written record is repaired by the sweep. Reports whether the chunks were
// stamped.
func (p *Pipeline) verifyProvenance(ctx context.Context, projectID, artifactID, sourceName, content string, opts IngestArtifactOptions) bool {
	if !opts.Document {
		// A workflow's output (a review, a research note) has no path in
		// any repository: never eligible.
		p.recordProvenanceOutcome(projectID, "not_document")
		return false
	}
	if p.cfg.Provenance == nil || p.cfg.RecordProvenance == nil {
		p.recordProvenanceOutcome(projectID, "no_verifier")
		return false
	}
	res := p.cfg.Provenance.Verify(ctx, ProvenanceRequest{
		ProjectID: projectID, RepoScope: opts.RepoScope, Path: sourceName, Content: []byte(content),
	})
	rec := ProvenanceRecord(projectID, artifactID, opts.RepoScope, res)
	if res.Verified() {
		// Three steps, so that neither crash window leaves a lie: the record
		// first, as pending (unverified, re-tried by the sweep); then the
		// stamp; then the record as verified. A crash after the first step
		// leaves a pending record the sweep verifies and stamps; a crash
		// after the second leaves stamped chunks and a pending record the
		// sweep confirms. There is never a stamp without a record.
		pending := *rec
		pending.State, pending.Reason, pending.VerifiedAt = ProvenanceUnverified, ProvReasonPendingStamp, nil
		rec = &pending
	}
	if err := p.cfg.RecordProvenance(ctx, rec); err != nil {
		p.logger.Warn().Err(err).Str("project_id", projectID).Str("artifact_id", artifactID).
			Msg("pipeline: provenance record write failed; chunks left unverified")
		p.recordProvenanceOutcome(projectID, "record_failed")
		return false
	}
	if !res.Verified() {
		p.recordProvenanceOutcome(projectID, res.Reason)
		if !ProvenanceUnexaminable(res.Reason) {
			return false
		}
		// #506 C4: no usable clone state exists for this repository, so the
		// document is stored as it was before #482, labelled so that it can
		// never retire a provenance-verified version (C5) and so the sweep
		// upgrades it once the repository can be examined.
		if err := p.indexer.MarkVerifiedByArtifact(ctx, projectID, artifactID, ValidatorRoleProvenanceUnavailable); err != nil {
			p.logger.Warn().Err(err).Str("project_id", projectID).Str("artifact_id", artifactID).
				Msg("pipeline: provenance-unavailable stamp failed; chunks left unverified")
			return false
		}
		return true
	}
	if err := p.indexer.MarkVerifiedByArtifact(ctx, projectID, artifactID, ValidatorRoleProvenance); err != nil {
		p.logger.Warn().Err(err).Str("project_id", projectID).Str("artifact_id", artifactID).
			Msg("pipeline: provenance verify-stamp failed; the record stays pending and the sweep re-verifies it")
		p.recordProvenanceOutcome(projectID, "stamp_failed")
		return false
	}
	if err := p.cfg.RecordProvenance(ctx, ProvenanceRecord(projectID, artifactID, opts.RepoScope, res)); err != nil {
		p.logger.Warn().Err(err).Str("project_id", projectID).Str("artifact_id", artifactID).
			Msg("pipeline: provenance record not confirmed after the stamp; the sweep confirms it")
	}
	p.recordProvenanceOutcome(projectID, ProvenanceVerified)
	return true
}

func (p *Pipeline) recordProvenanceOutcome(projectID, outcome string) {
	if p.cfg.Metrics != nil && p.cfg.Metrics.PipelineProvenanceChecksTotal != nil {
		p.cfg.Metrics.PipelineProvenanceChecksTotal.WithLabelValues(projectID, outcome).Inc()
	}
}

// ProvenanceRecord is the stored form of a verifier result.
func ProvenanceRecord(projectID, artifactID, repoScope string, res ProvenanceResult) *persistence.ChunkProvenance {
	rec := &persistence.ChunkProvenance{
		ProjectID: projectID, ArtifactID: artifactID, RepoScope: repoScope, DocumentPath: res.Path,
		CommitSHA: res.CommitSHA, ContentBlobSHA: res.ContentBlobSHA, OriginBlobSHA: res.OriginBlobSHA,
		State: res.State, Reason: res.Reason, CheckedAt: res.CheckedAt,
	}
	if res.Verified() {
		at := res.CheckedAt
		rec.VerifiedAt = &at
	}
	return rec
}

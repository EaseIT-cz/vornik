package auditredact

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// AutonomyEvaluations decorates a persistence.AutonomyEvaluationRepository so
// an observe-mode proposal is scanned before it is stored (design 11,
// amendment 2026-11-20, B4; GitHub #602). The WOULD_START row carries the
// prompt the tick would have submitted, which is model text; the reason is
// free text built from gate reasons and errors, so it is scanned too. Every
// tick writes through Record, so one decorator covers every outcome. Always
// installed: a bypass carries a reason and counts every write.
type AutonomyEvaluations struct {
	inner    persistence.AutonomyEvaluationRepository
	detector secrets.Detector
	audit    persistence.SecretRedactionAuditRepository
	logger   *zerolog.Logger

	bypassReason Reason
	metrics      *Metrics
}

// NewAutonomyEvaluations wraps inner. A nil detector stores rows unscanned
// and counts each write as skipped with reason detector_nil.
func NewAutonomyEvaluations(inner persistence.AutonomyEvaluationRepository, detector secrets.Detector, audit persistence.SecretRedactionAuditRepository, logger *zerolog.Logger) *AutonomyEvaluations {
	return &AutonomyEvaluations{inner: inner, detector: detector, audit: audit, logger: logger}
}

// NewAutonomyEvaluationsBypassed is the decorator installed when scanning is
// off or the detector failed to build: it scans nothing and counts every
// write.
func NewAutonomyEvaluationsBypassed(inner persistence.AutonomyEvaluationRepository, reason Reason, logger *zerolog.Logger) *AutonomyEvaluations {
	return &AutonomyEvaluations{inner: inner, bypassReason: reason, logger: logger}
}

// SetMetrics injects the shared census holder.
func (r *AutonomyEvaluations) SetMetrics(m *Metrics) { r.metrics = m }

// BypassReason reports why this decorator scans nothing, or ReasonNone.
func (r *AutonomyEvaluations) BypassReason() Reason {
	if r == nil {
		return ReasonNone
	}
	return r.bypassReason
}

// Inner returns the undecorated repository.
func (r *AutonomyEvaluations) Inner() persistence.AutonomyEvaluationRepository { return r.inner }

// Record scans the proposed prompt and the reason, stores a copy with each
// finding replaced by its marker, and records the counts. The caller's
// struct is not edited; the daemon-set fields are not touched.
func (r *AutonomyEvaluations) Record(ctx context.Context, e *persistence.AutonomyEvaluation) error {
	if e == nil || r.detector == nil {
		reason := r.bypassReason
		if reason == ReasonNone {
			reason = ReasonDetectorNil
		}
		r.metrics.recordSeam(StoreAutonomyEvaluations, StatusSkipped, reason)
		return r.inner.Record(ctx, e)
	}
	clean := *e
	s := newTranscriptScan(r.detector, false)
	clean.ProposedPrompt = s.text(clean.ProposedPrompt)
	clean.Reason = s.text(clean.Reason)
	if s.total() > 0 {
		if r.logger != nil {
			r.logger.Warn().Str("project_id", clean.ProjectID).Interface("by_type", s.counts).
				Str("checkpoint", secrets.CheckpointToolAudit).
				Msg("secrets: autonomy evaluation scanned — redacting before persist")
		}
		r.metrics.recordSeam(StoreAutonomyEvaluations, StatusRedacted, ReasonNone)
	} else {
		r.metrics.recordSeam(StoreAutonomyEvaluations, StatusScanned, ReasonNone)
	}
	recordEvents(ctx, r.audit, r.logger, deref(clean.TaskID), s.counts, "secrets: recording autonomy-evaluation redaction events failed")
	err := r.inner.Record(ctx, &clean)
	e.CreatedAt = clean.CreatedAt
	return err
}

// List delegates unchanged: rows are stored redacted.
func (r *AutonomyEvaluations) List(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	return r.inner.List(ctx, filter)
}

// CountByOutcome delegates unchanged.
func (r *AutonomyEvaluations) CountByOutcome(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
	return r.inner.CountByOutcome(ctx, projectID, since, until)
}

// LatestByProject delegates unchanged.
func (r *AutonomyEvaluations) LatestByProject(ctx context.Context) (map[string]*persistence.AutonomyEvaluation, error) {
	return r.inner.LatestByProject(ctx)
}

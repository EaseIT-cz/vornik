package auditredact

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// RetrievalAudit decorates a persistence.MemoryRetrievalAuditRepository so the
// recall query a person or a model typed is scanned before it is stored
// (chat-audit retention and redaction design §12, GitHub #680, operator
// decision O-549-1). Every recall path writes through Record, so one
// decorator covers companion recall, memory_search, the UI and the API.
// Always installed: a bypass carries a reason and counts every write.
type RetrievalAudit struct {
	inner    persistence.MemoryRetrievalAuditRepository
	detector secrets.Detector
	audit    persistence.SecretRedactionAuditRepository
	logger   *zerolog.Logger

	bypassReason Reason
	metrics      *Metrics
}

// NewRetrievalAudit wraps inner. A nil detector stores queries unscanned and
// counts each write as skipped with reason detector_nil.
func NewRetrievalAudit(inner persistence.MemoryRetrievalAuditRepository, detector secrets.Detector, audit persistence.SecretRedactionAuditRepository, logger *zerolog.Logger) *RetrievalAudit {
	return &RetrievalAudit{inner: inner, detector: detector, audit: audit, logger: logger}
}

// NewRetrievalAuditBypassed is the decorator installed when scanning is off or
// the detector failed to build: it scans nothing and counts every write.
func NewRetrievalAuditBypassed(inner persistence.MemoryRetrievalAuditRepository, reason Reason, logger *zerolog.Logger) *RetrievalAudit {
	return &RetrievalAudit{inner: inner, bypassReason: reason, logger: logger}
}

// SetMetrics injects the shared census holder.
func (r *RetrievalAudit) SetMetrics(m *Metrics) { r.metrics = m }

// BypassReason reports why this decorator scans nothing, or ReasonNone.
func (r *RetrievalAudit) BypassReason() Reason {
	if r == nil {
		return ReasonNone
	}
	return r.bypassReason
}

// Inner returns the undecorated repository.
func (r *RetrievalAudit) Inner() persistence.MemoryRetrievalAuditRepository { return r.inner }

// Record scans the query, stores a copy with each finding replaced by its
// marker, and records the counts. The caller's struct is not edited; the
// daemon-set fields (ids, chunk ids, scores, actor) are not touched.
func (r *RetrievalAudit) Record(ctx context.Context, a *persistence.MemoryRetrievalAudit) error {
	if a == nil || r.detector == nil {
		reason := r.bypassReason
		if reason == ReasonNone {
			reason = ReasonDetectorNil
		}
		r.metrics.recordSeam(StoreMemoryRetrievalAudit, StatusSkipped, reason)
		return r.inner.Record(ctx, a)
	}
	clean := *a
	s := newTranscriptScan(r.detector, false)
	clean.Query = s.text(clean.Query)
	if s.total() > 0 {
		if r.logger != nil {
			r.logger.Warn().Str("project_id", clean.ProjectID).Interface("by_type", s.counts).
				Str("checkpoint", secrets.CheckpointToolAudit).
				Msg("secrets: memory recall query scanned — redacting before persist")
		}
		r.metrics.recordSeam(StoreMemoryRetrievalAudit, StatusRedacted, ReasonNone)
	} else {
		r.metrics.recordSeam(StoreMemoryRetrievalAudit, StatusScanned, ReasonNone)
	}
	recordEvents(ctx, r.audit, r.logger, deref(clean.TaskID), s.counts, "secrets: recording memory-recall redaction events failed")
	err := r.inner.Record(ctx, &clean)
	a.ID = clean.ID
	return err
}

// FeedbackStats delegates unchanged.
func (r *RetrievalAudit) FeedbackStats(ctx context.Context, projectID string, since time.Time) (*persistence.MemoryFeedbackStats, error) {
	return r.inner.FeedbackStats(ctx, projectID, since)
}

// UnretrievedChunkIDs delegates unchanged.
func (r *RetrievalAudit) UnretrievedChunkIDs(ctx context.Context, projectID string, since time.Time, limit int) ([]string, error) {
	return r.inner.UnretrievedChunkIDs(ctx, projectID, since, limit)
}

// List delegates unchanged: queries are stored redacted.
func (r *RetrievalAudit) List(ctx context.Context, filter persistence.MemoryRetrievalAuditFilter) ([]*persistence.MemoryRetrievalAudit, error) {
	return r.inner.List(ctx, filter)
}

// A2AChunksByCallerKey delegates unchanged.
func (r *RetrievalAudit) A2AChunksByCallerKey(ctx context.Context, since time.Time) ([]persistence.A2AKeyRetrieval, error) {
	return r.inner.A2AChunksByCallerKey(ctx, since)
}

// deref is the task id for the redaction event, or "" for a recall outside a task.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

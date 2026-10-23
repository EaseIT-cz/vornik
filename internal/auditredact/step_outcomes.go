package auditredact

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// StepOutcomeStore is what the StepOutcomes decorator wraps: the outcome
// repository plus the three optional methods callers reach by type assertion
// — the watchdog's pending sweep and stuck-step recorder, and the executor's
// orphan sweep. Both production repositories satisfy it. A decorator that
// wrapped only the wide interface would hide them, and each assertion would
// fail quietly and switch a backstop off (chat-audit retention and redaction
// design §11.1.1, §11.2.1, GitHub #425).
type StepOutcomeStore interface {
	persistence.ExecutionStepOutcomeRepository
	RecordStuckStepOutcome(ctx context.Context, o *persistence.ExecutionStepOutcome, since time.Time) (bool, error)
	SweepPendingForTerminalExecutions(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
	SweepPendingForTaskOrphans(ctx context.Context, taskID string) (int64, error)
}

// StepOutcomes decorates the step-outcome repository so the one column that
// quotes model output — hallucination_signals — is scanned before it is
// stored (design §11.2.1, GitHub #425).
//
// A signal quotes the step's result after the result_json sink, which leaves a
// credential intact under `detect` or with scanning off, and one rule quotes a
// stored tool input. The column gets the chat-audit signal walk: the full
// detector, the fragment rule and the edge-run rule. It always redacts,
// whatever the result_json action: that action keeps result.json intact for
// inspection, not a second copy of a credential in an audit column.
//
// Always installed: a bypass carries a reason and counts every Record.
type StepOutcomes struct {
	StepOutcomeStore
	detector secrets.Detector
	audit    persistence.SecretRedactionAuditRepository
	logger   *zerolog.Logger

	bypassReason Reason
	metrics      *Metrics
}

// NewStepOutcomes wraps inner. A nil detector stores rows unscanned and counts
// each Record as skipped with reason detector_nil.
func NewStepOutcomes(inner StepOutcomeStore, detector secrets.Detector, audit persistence.SecretRedactionAuditRepository, logger *zerolog.Logger) *StepOutcomes {
	return &StepOutcomes{StepOutcomeStore: inner, detector: detector, audit: audit, logger: logger}
}

// NewStepOutcomesBypassed is the decorator installed when scanning is off or
// the detector failed to build: it scans nothing and counts every Record.
func NewStepOutcomesBypassed(inner StepOutcomeStore, reason Reason, logger *zerolog.Logger) *StepOutcomes {
	return &StepOutcomes{StepOutcomeStore: inner, bypassReason: reason, logger: logger}
}

// SetMetrics injects the shared census holder.
func (s *StepOutcomes) SetMetrics(m *Metrics) { s.metrics = m }

// BypassReason reports why this decorator scans nothing, or ReasonNone.
func (s *StepOutcomes) BypassReason() Reason {
	if s == nil {
		return ReasonNone
	}
	return s.bypassReason
}

// Inner returns the wrapped repository, for the container's bypass upgrade.
func (s *StepOutcomes) Inner() StepOutcomeStore { return s.StepOutcomeStore }

// Record scans a copy of o's hallucination signals and stores the copy. The
// caller's struct is never edited, except for the fields the repository
// assigns (RecordedAt on SQLite), which are copied back.
func (s *StepOutcomes) Record(ctx context.Context, o *persistence.ExecutionStepOutcome) error {
	if o == nil {
		return s.StepOutcomeStore.Record(ctx, o)
	}
	if s.detector == nil {
		reason := s.bypassReason
		if reason == ReasonNone {
			reason = ReasonDetectorNil
		}
		s.metrics.recordSeam(StoreExecutionStepOutcomes, StatusSkipped, reason)
		return s.StepOutcomeStore.Record(ctx, o)
	}
	clean := *o
	scan := newTranscriptScan(s.detector, false)
	if len(o.HallucinationSignals) > 0 {
		if out := signals(scan, string(o.HallucinationSignals)); out != string(o.HallucinationSignals) {
			clean.HallucinationSignals = []byte(out)
		}
	}
	if scan.total() > 0 {
		if s.logger != nil {
			s.logger.Warn().Str("task_id", o.TaskID).Str("step_id", o.StepID).
				Interface("by_type", scan.counts).Str("checkpoint", secrets.CheckpointToolAudit).
				Msg("secrets: step outcome hallucination signals scanned — redacting before persist")
		}
		s.metrics.recordSeam(StoreExecutionStepOutcomes, StatusRedacted, ReasonNone)
	} else {
		s.metrics.recordSeam(StoreExecutionStepOutcomes, StatusScanned, ReasonNone)
	}
	recordEvents(ctx, s.audit, s.logger, o.TaskID, scan.counts, "secrets: recording step-outcome redaction events failed")
	err := s.StepOutcomeStore.Record(ctx, &clean)
	o.RecordedAt = clean.RecordedAt
	return err
}

var (
	_ persistence.ExecutionStepOutcomeRepository = (*StepOutcomes)(nil)
	_ StepOutcomeStore                           = (*StepOutcomes)(nil)
)

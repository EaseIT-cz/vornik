package auditredact

import (
	"context"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// TaskMessages decorates a persistence.TaskMessageRepository so every message
// is scanned before it is stored (chat-audit retention and redaction design
// §9.2.3, GitHub #395).
//
// task_messages had no write-time scan at all: 25 insert sites across the API,
// UI, Telegram, scheduler, steering, taint lineage and the executor, and the
// REST writer stored caller-supplied metadata verbatim. One decorator at the
// repository covers every one of them, including writers not yet written.
//
// The two parts get different rules, because they are read differently:
//   - content is prose a person or a model wrote — the full detector, the same
//     rule as chat_audit_log.user_message;
//   - metadata is machine-read state (the taint latch's source_set_hash,
//     option ids, choice, step_id) — the detector WITHOUT the entropy class,
//     whose false positives would break a state machine, plus the key-name
//     rule for a JSON "password": "…" pair that generic_kv cannot see.
//     Clean metadata is stored byte-identical.
//
// Always installed: a bypass carries a reason and counts every write.
type TaskMessages struct {
	inner    persistence.TaskMessageRepository
	detector secrets.Detector
	audit    persistence.SecretRedactionAuditRepository
	logger   *zerolog.Logger

	bypassReason Reason
	metrics      *Metrics
}

// NewTaskMessages wraps inner. A nil detector stores messages unscanned and
// counts each write as skipped with reason detector_nil.
func NewTaskMessages(inner persistence.TaskMessageRepository, detector secrets.Detector, audit persistence.SecretRedactionAuditRepository, logger *zerolog.Logger) *TaskMessages {
	return &TaskMessages{inner: inner, detector: detector, audit: audit, logger: logger}
}

// NewTaskMessagesBypassed is the decorator installed when scanning is off or
// the detector failed to build: it scans nothing and counts every write.
func NewTaskMessagesBypassed(inner persistence.TaskMessageRepository, reason Reason, logger *zerolog.Logger) *TaskMessages {
	return &TaskMessages{inner: inner, bypassReason: reason, logger: logger}
}

// SetMetrics injects the shared census holder.
func (m *TaskMessages) SetMetrics(metrics *Metrics) { m.metrics = metrics }

// BypassReason reports why this decorator scans nothing, or ReasonNone.
func (m *TaskMessages) BypassReason() Reason {
	if m == nil {
		return ReasonNone
	}
	return m.bypassReason
}

// Inner returns the wrapped repository, for the container's bypass upgrade.
func (m *TaskMessages) Inner() persistence.TaskMessageRepository { return m.inner }

// Insert scans a copy of msg and stores it. The repository assigns the ID on
// the struct it is handed; it is copied back, because callers read msg.ID
// after Insert (a checkpoint's id becomes the task's open_checkpoint_id).
func (m *TaskMessages) Insert(ctx context.Context, msg *persistence.TaskMessage) error {
	if msg == nil {
		return m.inner.Insert(ctx, msg)
	}
	if m.detector == nil {
		reason := m.bypassReason
		if reason == ReasonNone {
			reason = ReasonDetectorNil
		}
		m.metrics.recordSeam(StoreTaskMessages, StatusSkipped, reason)
		return m.inner.Insert(ctx, msg)
	}
	clean := *msg
	content := newTranscriptScan(m.detector, false)
	clean.Content = content.text(msg.Content)
	meta := newTranscriptScan(m.detector, true)
	clean.Metadata = metadata(meta, msg.Metadata)
	// The identity rule (design §10.2.3, GitHub #414): the REST authorId is
	// the caller's string. Scanned with the metadata scan, so without entropy.
	if msg.AuthorID != nil {
		author := meta.text(*msg.AuthorID)
		clean.AuthorID = &author
	}

	counts := content.counts
	for k, n := range meta.counts {
		counts[k] += n
	}
	if len(counts) > 0 {
		if m.logger != nil {
			m.logger.Warn().Str("task_id", msg.TaskID).Str("message_kind", msg.MessageKind).
				Interface("by_type", counts).Str("checkpoint", secrets.CheckpointToolAudit).
				Msg("secrets: task message scanned — redacting before persist")
		}
		m.metrics.recordSeam(StoreTaskMessages, StatusRedacted, ReasonNone)
	} else {
		m.metrics.recordSeam(StoreTaskMessages, StatusScanned, ReasonNone)
	}
	recordEvents(ctx, m.audit, m.logger, msg.TaskID, counts, "secrets: recording task-message redaction events failed")
	err := m.inner.Insert(ctx, &clean)
	msg.ID = clean.ID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = clean.CreatedAt
	}
	return err
}

// List delegates unchanged: messages are stored redacted.
func (m *TaskMessages) List(ctx context.Context, f persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return m.inner.List(ctx, f)
}

// GetOpenCheckpoint delegates unchanged.
func (m *TaskMessages) GetOpenCheckpoint(ctx context.Context, taskID string) (*persistence.TaskMessage, error) {
	return m.inner.GetOpenCheckpoint(ctx, taskID)
}

// MarkCheckpointResolved delegates unscanned: it merges only the constant
// resolved:true into a row that was scanned when it was inserted.
func (m *TaskMessages) MarkCheckpointResolved(ctx context.Context, taskID, checkpointID string) error {
	return m.inner.MarkCheckpointResolved(ctx, taskID, checkpointID)
}

var _ persistence.TaskMessageRepository = (*TaskMessages)(nil)

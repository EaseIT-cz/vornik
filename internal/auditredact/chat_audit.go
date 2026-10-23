package auditredact

import (
	"context"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// ChatAudit decorates a persistence.ChatAuditRepository so everything the chat
// path persists is scanned and redacted BEFORE it is stored, at the one seam
// every writer passes through (chat-audit retention and redaction design §3,
// and §9 for what changed on 2026-10-16, GitHub #395).
//
// Two writers reach this store — the dispatcher's turn audit and the chat
// proxy's — and neither scanned. That is the same shape as the three
// tool-audit writers of which only one did, which is the incident the
// repository-boundary rule came from (2026-08-20). Every code path that
// rebuilds the repository set must re-apply this decoration.
//
// Since #395 the seam also owns the CUT: writers hand over each field capped
// at a scan window, and Insert scans the whole window before cutting it to the
// stored limit (§9.2.5). It is always installed — a bypass carries a reason
// and counts every write — because an absent decorator cannot count the rows
// it lets through (§9.2.1).
type ChatAudit struct {
	inner    persistence.ChatAuditRepository
	detector secrets.Detector
	audit    persistence.SecretRedactionAuditRepository
	logger   *zerolog.Logger

	bypassReason Reason
	metrics      *Metrics
}

// NewChatAudit wraps inner. A nil detector stores content unscanned (still
// cut) and counts each write as skipped with reason detector_nil, so CE paths
// and tests that never wire secret scanning still persist chat audit.
func NewChatAudit(inner persistence.ChatAuditRepository, detector secrets.Detector, audit persistence.SecretRedactionAuditRepository, logger *zerolog.Logger) *ChatAudit {
	return &ChatAudit{inner: inner, detector: detector, audit: audit, logger: logger}
}

// NewChatAuditBypassed returns the decorator the container installs when
// scanning is off or the detector failed to build: it scans nothing, still
// owns the cut, and counts every write as skipped with reason.
func NewChatAuditBypassed(inner persistence.ChatAuditRepository, reason Reason, logger *zerolog.Logger) *ChatAudit {
	return &ChatAudit{inner: inner, bypassReason: reason, logger: logger}
}

// SetMetrics injects the shared census holder.
func (c *ChatAudit) SetMetrics(m *Metrics) { c.metrics = m }

// BypassReason reports why this decorator scans nothing, or ReasonNone.
func (c *ChatAudit) BypassReason() Reason {
	if c == nil {
		return ReasonNone
	}
	return c.bypassReason
}

// skipReason is the reason a write is counted skipped with.
func (c *ChatAudit) skipReason() Reason {
	if c.bypassReason != ReasonNone {
		return c.bypassReason
	}
	return ReasonDetectorNil
}

// RedactText scans body and returns it with every finding replaced by a
// [REDACTED:type] marker, plus the per-type counts.
func (c *ChatAudit) RedactText(body string) (string, map[string]int) {
	if c == nil || c.detector == nil || body == "" {
		return body, nil
	}
	findings := c.detector.Scan([]byte(body))
	if len(findings) == 0 {
		return body, nil
	}
	return string(secrets.Redact([]byte(body), findings)), secrets.CountByType(findings)
}

// SavePrompt redacts, then stores; the hash the inner repository returns is of
// what was stored, because it hashes the body it is handed (design §3.1).
func (c *ChatAudit) SavePrompt(ctx context.Context, body string) (string, error) {
	if c.detector == nil {
		c.metrics.recordSeam(StoreChatSystemPrompts, StatusSkipped, c.skipReason())
		return c.inner.SavePrompt(ctx, body)
	}
	clean, counts := c.RedactText(body)
	if len(counts) > 0 {
		if c.logger != nil {
			c.logger.Warn().Interface("by_type", counts).
				Str("checkpoint", secrets.CheckpointToolAudit).
				Msg("secrets: chat system prompt scanned — redacting before persist")
		}
		c.metrics.recordSeam(StoreChatSystemPrompts, StatusRedacted, ReasonNone)
	} else {
		c.metrics.recordSeam(StoreChatSystemPrompts, StatusScanned, ReasonNone)
	}
	c.record(ctx, counts)
	return c.inner.SavePrompt(ctx, clean)
}

// Insert scans the row's text, then cuts it to the stored limits, then stores.
//
// ChatID and UserID get the identity rule (design §10.2.3, GitHub #414): the
// detector without the entropy class, each finding replaced in place so a
// "<channel>:" prefix survives. Both are load-bearing for origin resolution
// (chatorigin routes a finished task's result by them), and both can be chosen
// by an outsider: the chat proxy's chat_id is "api:" plus the caller's
// User-Agent, and a mail turn's user_id is the inbound From address. A routing
// key that holds a credential's shape is the defect, not a delivery to keep;
// no channel id matches a pattern without entropy. ID, ProjectID, RoleUsed and
// Model are set by the daemon and never touched.
//
// The caller's struct is never edited — the same rule as Repo.Log. A decorator
// that edits its argument makes the redaction visible at a distance. Shallow
// copy is sufficient while every field touched here is a string.
func (c *ChatAudit) Insert(ctx context.Context, entry *persistence.ChatAuditEntry) error {
	if entry == nil {
		return c.inner.Insert(ctx, entry)
	}
	clean := *entry
	if c.detector == nil {
		// Unscanned, but the cut is the seam's, so it still happens.
		clean.UserMessage = Cut(clean.UserMessage, ChatTextLimit)
		clean.Response = Cut(clean.Response, ChatTextLimit)
		clean.ToolCallsJSON = toolCalls(nil, clean.ToolCallsJSON)
		c.metrics.recordSeam(StoreChatAuditLog, StatusSkipped, c.skipReason())
		err := c.inner.Insert(ctx, &clean)
		entry.ID = clean.ID
		return err
	}
	// Scan first, cut second (§9.2.5). The order of the fields matters only
	// for the signals: they are scanned last, so every finding in the reply,
	// the message and the tool fields is a known match for the fragment rule.
	s := newTranscriptScan(c.detector, false)
	ids := newTranscriptScan(c.detector, true)
	clean.ChatID = ids.text(clean.ChatID)
	clean.UserID = ids.text(clean.UserID)
	for k, n := range ids.counts {
		s.counts[k] += n
	}
	clean.UserMessage = Cut(s.text(clean.UserMessage), ChatTextLimit)
	clean.Response = Cut(s.text(clean.Response), ChatTextLimit)
	clean.ToolCallsJSON = toolCalls(s, clean.ToolCallsJSON)
	clean.HallucinationSignalsJSON = signals(s, clean.HallucinationSignalsJSON)
	if s.total() > 0 {
		if c.logger != nil {
			c.logger.Warn().Str("chat_id", clean.ChatID).Interface("by_type", s.counts).
				Str("checkpoint", secrets.CheckpointToolAudit).
				Msg("secrets: chat audit row scanned — redacting before persist")
		}
		c.metrics.recordSeam(StoreChatAuditLog, StatusRedacted, ReasonNone)
	} else {
		c.metrics.recordSeam(StoreChatAuditLog, StatusScanned, ReasonNone)
	}
	c.record(ctx, s.counts)
	err := c.inner.Insert(ctx, &clean)
	entry.ID = clean.ID
	return err
}

// GetPrompt delegates unchanged: bodies are stored redacted.
func (c *ChatAudit) GetPrompt(ctx context.Context, hash string) (string, error) {
	return c.inner.GetPrompt(ctx, hash)
}

// List delegates unchanged.
func (c *ChatAudit) List(ctx context.Context, filter persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	return c.inner.List(ctx, filter)
}

// GetByID delegates unchanged.
func (c *ChatAudit) GetByID(ctx context.Context, id string) (*persistence.ChatAuditEntry, error) {
	return c.inner.GetByID(ctx, id)
}

// GetChatAuditsByTurnIDs delegates unchanged.
func (c *ChatAudit) GetChatAuditsByTurnIDs(ctx context.Context, turnIDs []string) (map[string]persistence.ChatAuditEntry, error) {
	return c.inner.GetChatAuditsByTurnIDs(ctx, turnIDs)
}

// Inner returns the wrapped repository — the idempotency and bypass-upgrade
// check the container's rebuild path needs, mirroring Repo.Inner.
func (c *ChatAudit) Inner() persistence.ChatAuditRepository { return c.inner }

func (c *ChatAudit) record(ctx context.Context, counts map[string]int) {
	recordEvents(ctx, c.audit, c.logger, "", counts, "secrets: recording chat-audit redaction events failed")
}

// recordEvents writes one secret_redaction_audit row per finding type, under
// the tool_audit checkpoint with source "live". Best-effort: the event row must
// never block the write it describes.
func recordEvents(ctx context.Context, audit persistence.SecretRedactionAuditRepository, logger *zerolog.Logger, taskID string, counts map[string]int, failMsg string) {
	if audit == nil || len(counts) == 0 {
		return
	}
	events := make([]persistence.SecretRedactionEvent, 0, len(counts))
	for findingType, n := range counts {
		if n > 0 {
			events = append(events, persistence.SecretRedactionEvent{
				TaskID: taskID, Checkpoint: secrets.CheckpointToolAudit, FindingType: findingType, Count: n, Source: "live",
			})
		}
	}
	if len(events) == 0 {
		return
	}
	if err := audit.Record(ctx, events); err != nil && logger != nil {
		logger.Warn().Err(err).Msg(failMsg)
	}
}

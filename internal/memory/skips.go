package memory

import (
	"context"
	"errors"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/ctxerr"
)

// Sites of vornik_memory_skipped_total (outcome-quality LLD §S8.11, GitHub
// #369): each is a memory step that failed and was skipped or allowed past.
const (
	skipSiteInlineTitle     = "inline_title"
	skipSiteDedupLookup     = "dedup_lookup"
	skipSiteClassifier      = "classifier"
	skipSiteRerank          = "rerank"
	skipSiteQuarantineWrite = "quarantine_write"
	skipSiteClaimAudit      = "claim_audit"
)

// llmSkipReason is the reason for a failed model call, spelled as the
// skill distiller and the judges spell it: llm_timeout, llm_cancelled or
// llm_error.
func llmSkipReason(ctx context.Context, err error) string {
	return "llm_" + string(ctxerr.Classify(ctx, err))
}

// isOptionalWorkOff reports the optional-work gate's refusal (breaker
// design §5.3d): a state the operator set, not a failure, so it is not
// counted as a skip.
func isOptionalWorkOff(err error) bool {
	return errors.Is(err, chat.ErrOptionalWorkDisabled)
}

// ErrClassifierUnparseable: the classifier model answered with no known
// class. ErrClassifierNoChoices: it answered with no choices. Both are told
// apart from a failed call (GitHub #369, §S8.11 7a).
var (
	ErrClassifierUnparseable = errors.New("classifier: unrecognised class in response")
	ErrClassifierNoChoices   = errors.New("classifier: no choices in response")
)

// classifierSkipReason is the memory_skipped_total reason for a failed
// inline classification.
func classifierSkipReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, ErrClassifierUnparseable):
		return "unparseable"
	case errors.Is(err, ErrClassifierNoChoices):
		return "no_choices"
	default:
		return llmSkipReason(ctx, err)
	}
}

// titleChunk generates and stores a chunk's display title. Display-only:
// a failure leaves the title NULL for the backfill, and is logged at Warn
// and counted, so a dead titler model is visible (GitHub #369, §S8.11 6a).
// Nil-safe when the titler is not wired.
func (w *Worker) titleChunk(ctx context.Context, chunkID, projectID, content string) {
	if w.titler == nil {
		return
	}
	title, err := w.titler.Title(ctx, content, projectID, chunkID)
	switch {
	case err != nil && isOptionalWorkOff(err):
		w.logger.Debug().Err(err).Str("chunk_id", chunkID).
			Msg("memory worker: title generation off (optional LLM work disabled) — leaving NULL")
	case err != nil:
		reason := llmSkipReason(ctx, err)
		w.logger.Warn().Err(err).Str("chunk_id", chunkID).Str("reason", reason).
			Msg("memory worker: title generation failed — leaving NULL for backfill")
		w.metrics.RecordSkipped(skipSiteInlineTitle, reason)
	case title != "":
		if uerr := w.repo.UpdateContentTitle(ctx, chunkID, title); uerr != nil {
			w.logger.Warn().Err(uerr).Str("chunk_id", chunkID).
				Msg("memory worker: content_title persist failed")
		}
	}
}

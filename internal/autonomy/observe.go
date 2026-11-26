package autonomy

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Observe mode (GitHub #602; design 11, amendment 2026-11-20, section B).
// createAutonomousTask makes the decision; these helpers keep it short.

// wouldStartScanLimit bounds the WOULD_START rows read per proposal (B3).
const wouldStartScanLimit = 500

// observeRepeat checks a proposal against the project's earlier WOULD_START
// rows inside the duplicate window (B3). No task row exists in observe
// mode, so the duplicate window over tasks never sees a proposal. It runs
// under m.mu with the task list the other gates read. stop reports that
// the caller must return now: a repeat (recorded DUPLICATE, nil error) or a
// failed read (recorded DB_ERROR, fail closed like the task-list read).
func (m *Manager) observeRepeat(ctx context.Context, project *registry.Project, taskType, workflowID, prompt string, evalStart time.Time) (stop bool, err error) {
	window := autonomyDuplicateWindow(project)
	if window <= 0 || m.evalRepo == nil {
		return false, nil // cron style: no duplicate check in either mode
	}
	outcome := persistence.AutonomyOutcomeWouldStart
	rows, lerr := m.evalRepo.List(ctx, persistence.AutonomyEvaluationFilter{
		ProjectID: &project.ID,
		Outcome:   &outcome,
		PageSize:  wouldStartScanLimit,
	})
	rec := evalRecord{projectID: project.ID, taskType: taskType, workflowID: workflowID, prompt: prompt, start: evalStart}
	if lerr != nil {
		rec.outcome = persistence.AutonomyOutcomeDBError
		rec.reason = "list earlier proposals: " + lerr.Error()
		m.recordEvaluation(ctx, rec)
		return true, fmt.Errorf("failed to inspect earlier proposals: %w", lerr)
	}
	reason, dup := findWouldStartDuplicate(rows, taskType, workflowID, prompt, window, time.Now())
	if !dup {
		return false, nil
	}
	m.logger.Info().Str("project", project.ID).Str("reason", reason).Msg("autonomy observe mode: proposal suppressed as duplicate")
	rec.outcome = persistence.AutonomyOutcomeDuplicate
	rec.reason = reason
	m.recordEvaluation(ctx, rec)
	return true, nil
}

// findWouldStartDuplicate reports a WOULD_START row inside window for the
// same task type, workflow and prompt. The prompt matches by hash (immune
// to the stored copy's redaction) or by the normalisation the task
// duplicate check uses.
func findWouldStartDuplicate(rows []*persistence.AutonomyEvaluation, taskType, workflowID, prompt string, window time.Duration, now time.Time) (string, bool) {
	hash := hashPrompt(prompt)
	normalized := normalizeAutonomyPrompt(prompt)
	for _, e := range rows {
		if now.Sub(e.CreatedAt) >= window {
			continue
		}
		if e.TaskType != taskType || e.WorkflowID != workflowID {
			continue
		}
		if e.PromptHash != hash && normalizeAutonomyPrompt(e.ProposedPrompt) != normalized {
			continue
		}
		return fmt.Sprintf("matching proposal %s recorded within the last %s", e.ID, window), true
	}
	return "", false
}

// observeReservationID is the ID observe mode reserves budget under (B2).
// No task will ever carry it, so the terminal-task sweep never settles it;
// settleObserveReservation does, and the stale sweep backs that up.
func observeReservationID(taskID string) string { return "observe-" + taskID }

// settleObserveReservation releases the booking observe mode made only to
// ask the ledger act mode's question. A failure is logged and left to
// SweepTerminalAndStale.
func (m *Manager) settleObserveReservation(ctx context.Context, projectID, reservationID string) {
	if _, err := m.reservRepo.SettleByTask(ctx, reservationID, time.Now().UTC()); err != nil {
		m.logger.Warn().Err(err).Str("project", projectID).Str("reservation", reservationID).
			Msg("autonomy observe mode: settling the budget probe failed; the stale sweep will settle it")
	}
}

package api

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// secretRedactionRecorder is the slice of
// persistence.SecretRedactionAuditRepository the API sinks need.
type secretRedactionRecorder interface {
	Record(ctx context.Context, events []persistence.SecretRedactionEvent) error
}

// SetSecretRedactionAudit wires the recorder for the webhook and
// backlog-deposit checkpoints. Nil-safe.
func (s *Server) SetSecretRedactionAudit(r secretRedactionRecorder) {
	s.secretRedactionAudit = r
}

// recordSecretFindings records one event per finding type at a checkpoint,
// with source "live", in every action branch: the audit records what the scan
// found. Best-effort: a failure is logged and never fails the request.
func (s *Server) recordSecretFindings(projectID, taskID, checkpoint string, counts map[string]int) {
	if s.secretRedactionAudit == nil || len(counts) == 0 {
		return
	}
	var events []persistence.SecretRedactionEvent
	for ft, n := range counts {
		if n <= 0 {
			continue
		}
		events = append(events, persistence.SecretRedactionEvent{
			ProjectID: projectID, TaskID: taskID,
			Checkpoint: checkpoint, FindingType: ft, Count: n, Source: "live",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.secretRedactionAudit.Record(ctx, events); err != nil {
		s.logger.Warn().Err(err).Str("checkpoint", checkpoint).
			Str("project_id", projectID).Str("task_id", taskID).
			Msg("secrets: failed to record redaction audit event(s) — badge/history will under-count")
	}
}

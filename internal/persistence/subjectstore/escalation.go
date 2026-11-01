package subjectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/datasubject"
)

// The once-only record of the Art 12(3) escalation pass (GitHub #344 slice
// 5d-1c; art17 redact design §15.16): one operator alert per erasure row and
// deadline. Written only after the alert was sent, so a failed send retries;
// keyed with the deadline, so an extension alerts again.

// EscalationRaised reports whether the alert for this row and deadline was
// sent.
func (r *Repository) EscalationRaised(ctx context.Context, k datasubject.EscalationKey) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM data_subject_escalations
		WHERE request_id = $1 AND row_table = $2 AND row_id = $3 AND deadline = $4`,
		k.RequestID, string(k.Table), k.RowID, r.d.bindTime(k.Deadline.UTC().Truncate(time.Microsecond))).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("subjectstore: read escalation: %w", r.d.mapErr(err))
	}
	return true, nil
}

// RecordEscalation records a sent alert. Recording one twice is not an error.
func (r *Repository) RecordEscalation(ctx context.Context, k datasubject.EscalationKey, raisedAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO data_subject_escalations
		(request_id, row_table, row_id, deadline, raised_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (request_id, row_table, row_id, deadline) DO NOTHING`,
		k.RequestID, string(k.Table), k.RowID, r.d.bindTime(k.Deadline.UTC().Truncate(time.Microsecond)), r.d.bindTime(raisedAt.UTC()))
	if err != nil {
		return fmt.Errorf("subjectstore: record escalation: %w", r.d.mapErr(err))
	}
	return nil
}

// TaskStatus reads one task's status for the escalation pass's re-check;
// "" when the task is gone.
func (r *Repository) TaskStatus(ctx context.Context, taskID string) (string, error) {
	var status string
	err := r.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("subjectstore: read task status: %w", r.d.mapErr(err))
	}
	return status, nil
}

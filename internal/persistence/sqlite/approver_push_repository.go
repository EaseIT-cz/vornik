package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Approver devices' Web Push subscriptions (GitHub #163, agent-administered
// design part 09 §9.3a B.3), on the device repository's struct.

var _ persistence.ApproverPushRepository = (*ApproverDeviceRepository)(nil)

const approverPushCols = `s.device_id, s.endpoint, s.p256dh, s.auth, s.created_at, s.last_sent_at, s.last_status`

func scanApproverPush(sc interface{ Scan(...interface{}) error }) (persistence.ApproverPushSubscriptionRow, error) {
	var (
		row     persistence.ApproverPushSubscriptionRow
		created sqlTime
		sent    sqlNullTime
	)
	if err := sc.Scan(&row.DeviceID, &row.Endpoint, &row.P256DH, &row.Auth, &created, &sent, &row.LastStatus); err != nil {
		return row, err
	}
	row.CreatedAt = created.Time
	if sent.Valid {
		t := sent.Time
		row.LastSentAt = &t
	}
	return row, nil
}

// UpsertPushSubscription implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) UpsertPushSubscription(ctx context.Context, row persistence.ApproverPushSubscriptionRow) error {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO approver_push_subscriptions (device_id, endpoint, p256dh, auth, created_at, last_sent_at, last_status)
		SELECT ?, ?, ?, ?, ?, NULL, '' WHERE EXISTS (SELECT 1 FROM approver_devices WHERE id = ? AND revoked_at IS NULL)
		ON CONFLICT (device_id) DO UPDATE SET endpoint = excluded.endpoint, p256dh = excluded.p256dh, auth = excluded.auth,
			created_at = excluded.created_at, last_sent_at = NULL, last_status = ''`,
		row.DeviceID, row.Endpoint, row.P256DH, row.Auth, sqliteTime(row.CreatedAt), row.DeviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// GetPushSubscription implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) GetPushSubscription(ctx context.Context, deviceID string) (*persistence.ApproverPushSubscriptionRow, error) {
	row, err := scanApproverPush(r.db.QueryRowContext(ctx,
		`SELECT `+approverPushCols+` FROM approver_push_subscriptions s WHERE s.device_id = ?`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// DeletePushSubscription implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) DeletePushSubscription(ctx context.Context, deviceID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM approver_push_subscriptions WHERE device_id = ?`, deviceID)
	return err
}

// ListActivePushSubscriptions implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) ListActivePushSubscriptions(ctx context.Context) ([]persistence.ApproverPushSubscriptionRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+approverPushCols+` FROM approver_push_subscriptions s
		JOIN approver_devices d ON d.id = s.device_id WHERE d.revoked_at IS NULL ORDER BY s.device_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.ApproverPushSubscriptionRow
	for rows.Next() {
		row, err := scanApproverPush(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// RecordPushResult implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) RecordPushResult(ctx context.Context, deviceID, status string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE approver_push_subscriptions SET last_sent_at = ?, last_status = ? WHERE device_id = ?`,
		sqliteTime(at), status, deviceID)
	return err
}

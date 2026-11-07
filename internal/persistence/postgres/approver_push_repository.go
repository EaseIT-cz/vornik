package postgres

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
		row  persistence.ApproverPushSubscriptionRow
		sent sql.NullTime
	)
	if err := sc.Scan(&row.DeviceID, &row.Endpoint, &row.P256DH, &row.Auth, &row.CreatedAt, &sent, &row.LastStatus); err != nil {
		return row, err
	}
	row.CreatedAt = row.CreatedAt.UTC()
	if sent.Valid {
		t := sent.Time.UTC()
		row.LastSentAt = &t
	}
	return row, nil
}

// UpsertPushSubscription implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) UpsertPushSubscription(ctx context.Context, row persistence.ApproverPushSubscriptionRow) error {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO approver_push_subscriptions (device_id, endpoint, p256dh, auth, created_at, last_sent_at, last_status)
		SELECT $1::text, $2::text, $3::text, $4::text, $5::timestamptz, NULL, '' WHERE EXISTS (SELECT 1 FROM approver_devices WHERE id = $1 AND revoked_at IS NULL)
		ON CONFLICT (device_id) DO UPDATE SET endpoint = excluded.endpoint, p256dh = excluded.p256dh, auth = excluded.auth,
			created_at = excluded.created_at, last_sent_at = NULL, last_status = ''`,
		row.DeviceID, row.Endpoint, row.P256DH, row.Auth, row.CreatedAt)
	if err != nil {
		return mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// GetPushSubscription implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) GetPushSubscription(ctx context.Context, deviceID string) (*persistence.ApproverPushSubscriptionRow, error) {
	row, err := scanApproverPush(r.db.QueryRowContext(ctx,
		`SELECT `+approverPushCols+` FROM approver_push_subscriptions s WHERE s.device_id = $1`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return &row, nil
}

// DeletePushSubscription implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) DeletePushSubscription(ctx context.Context, deviceID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM approver_push_subscriptions WHERE device_id = $1`, deviceID)
	return mapDBError(err)
}

// ListActivePushSubscriptions implements persistence.ApproverPushRepository.
func (r *ApproverDeviceRepository) ListActivePushSubscriptions(ctx context.Context) ([]persistence.ApproverPushSubscriptionRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+approverPushCols+` FROM approver_push_subscriptions s
		JOIN approver_devices d ON d.id = s.device_id WHERE d.revoked_at IS NULL ORDER BY s.device_id`)
	if err != nil {
		return nil, mapDBError(err)
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
	_, err := r.db.ExecContext(ctx, `UPDATE approver_push_subscriptions SET last_sent_at = $1, last_status = $2 WHERE device_id = $3`,
		at, status, deviceID)
	return mapDBError(err)
}

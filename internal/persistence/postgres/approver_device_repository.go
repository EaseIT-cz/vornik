package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ApproverDeviceRepository implements persistence.ApproverDeviceRepository
// over Postgres. It mirrors the SQLite implementation;
// repotest.RunApproverDeviceSuite keeps the two honest.
type ApproverDeviceRepository struct {
	db persistence.DBTX
	// afterCount runs inside RedeemPairing once the device count is read;
	// a test seam that makes the first-device race deterministic.
	afterCount func()
}

// NewApproverDeviceRepository wires the repository.
func NewApproverDeviceRepository(db persistence.DBTX) *ApproverDeviceRepository {
	return &ApproverDeviceRepository{db: db}
}

var _ persistence.ApproverDeviceRepository = (*ApproverDeviceRepository)(nil)

// SetRedeemHookForTest installs a function RedeemPairing calls right after it
// reads the device count. Production code never sets it.
func (r *ApproverDeviceRepository) SetRedeemHookForTest(fn func()) { r.afterCount = fn }

// approverFirstDeviceLock serialises the "is there a device yet?" test with
// the insert that answers it (the budget-reservation pattern). It is the
// control: without it, repotest's deterministic race made 4 of 4 concurrent
// redeemers "first" in 5 of 5 runs (plan amendment 11). Taking it on every
// redeem, not only while no device exists, is deliberate: deciding to skip it
// would need the very count it protects. Pairings are rare.
const approverFirstDeviceLock = "approver_devices:first"

// CreatePairing implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) CreatePairing(ctx context.Context, p persistence.ApproverPairingRow) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO approver_pairings (code_hash, label, created_at, expires_at)
		VALUES ($1, $2, $3, $4)`, p.CodeHash, p.Label, p.CreatedAt, p.ExpiresAt)
	return mapDBError(err)
}

// withTx runs fn in a transaction when the handle is a pool, or directly on
// the caller's transaction when it already is one.
func (r *ApproverDeviceRepository) withTx(ctx context.Context, fn func(x persistence.DBTX) error) error {
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapDBError(err)
	}
	if !ok {
		return fn(r.db)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	committed = true
	return mapDBError(tx.Commit())
}

// GetPairing implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) GetPairing(ctx context.Context, codeHash string, now time.Time) (*persistence.ApproverPairingRow, error) {
	var p persistence.ApproverPairingRow
	err := r.db.QueryRowContext(ctx, `
		SELECT code_hash, label, created_at, expires_at FROM approver_pairings
		WHERE code_hash = $1 AND redeemed_at IS NULL AND expires_at > $2`, codeHash, now).
		Scan(&p.CodeHash, &p.Label, &p.CreatedAt, &p.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return &p, nil
}

// RedeemPairing implements persistence.ApproverDeviceRepository. A
// transaction-scoped advisory lock is taken before the device count is read,
// so two redeemers cannot both see an empty device set.
func (r *ApproverDeviceRepository) RedeemPairing(ctx context.Context, codeHash, claimHash string, d persistence.ApproverDeviceRow, req persistence.AgentApprovalRequestRow, now time.Time) (bool, error) {
	first := false
	err := r.withTx(ctx, func(x persistence.DBTX) error {
		if _, err := x.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, approverFirstDeviceLock); err != nil {
			return mapDBError(err)
		}
		res, err := x.ExecContext(ctx, `
			UPDATE approver_pairings SET redeemed_at = $1
			WHERE code_hash = $2 AND redeemed_at IS NULL AND expires_at > $1`, now, codeHash)
		if err != nil {
			return mapDBError(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return persistence.ErrNotFound
		}
		var active int
		if err := x.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_devices WHERE revoked_at IS NULL`).Scan(&active); err != nil {
			return mapDBError(err)
		}
		if r.afterCount != nil {
			r.afterCount()
		}
		first = active == 0
		if first {
			if err := insertApproverDevice(ctx, x, d); err != nil {
				return err
			}
			_, err := x.ExecContext(ctx, `UPDATE approver_pairings SET device_id = $1 WHERE code_hash = $2`, d.ID, codeHash)
			return mapDBError(err)
		}
		if err := insertApprovalRequest(ctx, x, req); err != nil {
			return err
		}
		_, err = x.ExecContext(ctx, `UPDATE approver_pairings SET claim_hash = $1, request_id = $2 WHERE code_hash = $3`, claimHash, req.ID, codeHash)
		return mapDBError(err)
	})
	if err != nil {
		return false, err
	}
	return first, nil
}

func insertApproverDevice(ctx context.Context, x persistence.DBTX, d persistence.ApproverDeviceRow) error {
	_, err := x.ExecContext(ctx, `
		INSERT INTO approver_devices (id, label, token_hash, paired_at, paired_by, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, d.ID, d.Label, d.TokenHash, d.PairedAt, d.PairedBy, d.LastUsedAt)
	return mapDBError(err)
}

func insertApprovalRequest(ctx context.Context, x persistence.DBTX, q persistence.AgentApprovalRequestRow) error {
	_, err := x.ExecContext(ctx, `
		INSERT INTO agent_approval_requests (id, namespace, kind, sentence, rendered, rendered_sha256, status, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		q.ID, q.Namespace, q.Kind, q.Sentence, string(q.Rendered), q.RenderedSHA256, q.Status, q.CreatedAt, q.ExpiresAt)
	return mapDBError(err)
}

// GetPairingByClaim implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) GetPairingByClaim(ctx context.Context, claimHash string) (*persistence.ApproverPairingRow, error) {
	var (
		p                  persistence.ApproverPairingRow
		redeemed           sql.NullTime
		claim, req, device sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT code_hash, label, created_at, expires_at, redeemed_at, claim_hash, request_id, device_id
		FROM approver_pairings WHERE claim_hash = $1`, claimHash).
		Scan(&p.CodeHash, &p.Label, &p.CreatedAt, &p.ExpiresAt, &redeemed, &claim, &req, &device)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if redeemed.Valid {
		t := redeemed.Time
		p.RedeemedAt = &t
	}
	p.ClaimHash, p.RequestID, p.DeviceID = claim.String, req.String, device.String
	return &p, nil
}

// CompletePairing implements persistence.ApproverDeviceRepository. The
// device_id IS NULL predicate makes it run once even under concurrent polls.
func (r *ApproverDeviceRepository) CompletePairing(ctx context.Context, claimHash string, d persistence.ApproverDeviceRow) error {
	return r.withTx(ctx, func(x persistence.DBTX) error {
		res, err := x.ExecContext(ctx, `UPDATE approver_pairings SET device_id = $1 WHERE claim_hash = $2 AND device_id IS NULL`, d.ID, claimHash)
		if err != nil {
			return mapDBError(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return persistence.ErrNotFound
		}
		return insertApproverDevice(ctx, x, d)
	})
}

// RemintEnrollmentToken implements persistence.ApproverDeviceRepository. One
// statement, so the guards and the swap are atomic.
func (r *ApproverDeviceRepository) RemintEnrollmentToken(ctx context.Context, claimHash, newHash string, notBefore time.Time) (*persistence.ApproverDeviceRow, error) {
	d, err := scanApproverDevice(r.db.QueryRowContext(ctx, `
		UPDATE approver_devices SET dead_token_hash = token_hash, dead_reason = 'confirmed', token_hash = $1
		WHERE id = (SELECT device_id FROM approver_pairings WHERE claim_hash = $2 AND device_id IS NOT NULL)
		  AND revoked_at IS NULL AND paired_at >= $3 AND last_used_at = paired_at
		  AND dead_token_hash IS NULL AND prev_token_hash IS NULL
		RETURNING `+approverDeviceCols, newHash, claimHash, notBefore))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return &d, nil
}

const approverDeviceCols = `id, label, token_hash, paired_at, paired_by, last_used_at, revoked_at,
	prev_token_hash, rotation_nonce, share_until, dead_token_hash, dead_reason, share_admitted, share_streak`

func scanApproverDevice(s interface{ Scan(...interface{}) error }) (persistence.ApproverDeviceRow, error) {
	var (
		d                         persistence.ApproverDeviceRow
		revoked, shareUntil       sql.NullTime
		prev, nonce, dead, reason sql.NullString
	)
	if err := s.Scan(&d.ID, &d.Label, &d.TokenHash, &d.PairedAt, &d.PairedBy, &d.LastUsedAt, &revoked,
		&prev, &nonce, &shareUntil, &dead, &reason, &d.ShareAdmitted, &d.ShareStreak); err != nil {
		return d, err
	}
	if revoked.Valid {
		t := revoked.Time
		d.RevokedAt = &t
	}
	if shareUntil.Valid {
		t := shareUntil.Time
		d.ShareUntil = &t
	}
	d.PrevTokenHash, d.RotationNonce, d.DeadTokenHash, d.DeadReason = prev.String, nonce.String, dead.String, reason.String
	return d, nil
}

// GetDeviceByTokenHash implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) GetDeviceByTokenHash(ctx context.Context, tokenHash string) (*persistence.ApproverDeviceRow, error) {
	d, err := scanApproverDevice(r.db.QueryRowContext(ctx, `SELECT `+approverDeviceCols+` FROM approver_devices
		WHERE token_hash = $1 OR prev_token_hash = $1 OR dead_token_hash = $1 LIMIT 1`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return &d, nil
}

// ListDevices implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ListDevices(ctx context.Context) ([]persistence.ApproverDeviceRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+approverDeviceCols+` FROM approver_devices ORDER BY paired_at, id`)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.ApproverDeviceRow
	for rows.Next() {
		d, err := scanApproverDevice(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, d)
	}
	return out, mapDBError(rows.Err())
}

// CountActiveDevices implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) CountActiveDevices(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_devices WHERE revoked_at IS NULL`).Scan(&n)
	return n, mapDBError(err)
}

// RotateToken implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) RotateToken(ctx context.Context, id, presentedHash, newHash, nonce string, shareUntil, now time.Time) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET
			dead_token_hash = CASE WHEN prev_token_hash IS NOT NULL THEN prev_token_hash ELSE dead_token_hash END,
			dead_reason = CASE WHEN prev_token_hash IS NOT NULL THEN 'confirmed' ELSE dead_reason END,
			share_streak = CASE WHEN share_admitted THEN share_streak ELSE 0 END,
			share_admitted = false,
			prev_token_hash = $1, token_hash = $2, rotation_nonce = $3, share_until = $4, last_used_at = $5
		WHERE id = $6 AND token_hash = $1 AND revoked_at IS NULL`,
		presentedHash, newHash, nonce, shareUntil, now, id)
	if err != nil {
		return mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// AdmitShare implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) AdmitShare(ctx context.Context, id, nonce string) (int, bool, error) {
	var streak int
	err := r.db.QueryRowContext(ctx, `
		UPDATE approver_devices SET share_admitted = true, share_streak = share_streak + 1
		WHERE id = $1 AND rotation_nonce = $2 AND NOT share_admitted
		RETURNING share_streak`, id, nonce).Scan(&streak)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, mapDBError(err)
	}
	return streak, true, nil
}

// ConfirmToken implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ConfirmToken(ctx context.Context, id, currentHash string) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET dead_token_hash = prev_token_hash, dead_reason = 'confirmed',
			prev_token_hash = NULL, rotation_nonce = NULL, share_until = NULL
		WHERE id = $1 AND token_hash = $2 AND prev_token_hash IS NOT NULL`, id, currentHash); err != nil {
		return mapDBError(err)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET dead_reason = 'confirmed'
		WHERE id = $1 AND token_hash = $2 AND prev_token_hash IS NULL AND dead_reason = 'expired'`, id, currentHash)
	return mapDBError(err)
}

// CloseShare implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) CloseShare(ctx context.Context, id, nonce string, until time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET share_until = $1
		WHERE id = $2 AND rotation_nonce = $3 AND share_until > $1`, until, id, nonce)
	return mapDBError(err)
}

// ExpireShares implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ExpireShares(ctx context.Context, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET dead_token_hash = prev_token_hash, dead_reason = 'expired',
			prev_token_hash = NULL, rotation_nonce = NULL, share_until = NULL
		WHERE prev_token_hash IS NOT NULL AND share_until <= $1`, now)
	return mapDBError(err)
}

// ResumeDevice implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ResumeDevice(ctx context.Context, codeHash, deadHash, newHash string, now time.Time) (*persistence.ApproverDeviceRow, error) {
	var out persistence.ApproverDeviceRow
	err := r.withTx(ctx, func(x persistence.DBTX) error {
		res, err := x.ExecContext(ctx, `
			UPDATE approver_pairings SET redeemed_at = $1
			WHERE code_hash = $2 AND redeemed_at IS NULL AND expires_at > $1`, now, codeHash)
		if err != nil {
			return mapDBError(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return persistence.ErrNotFound
		}
		d, err := scanApproverDevice(x.QueryRowContext(ctx, `
			UPDATE approver_devices SET token_hash = $1, last_used_at = $2,
				prev_token_hash = NULL, rotation_nonce = NULL, share_until = NULL,
				dead_token_hash = NULL, dead_reason = NULL, share_admitted = false, share_streak = 0
			WHERE dead_token_hash = $3 AND dead_reason = 'expired' AND revoked_at IS NULL
			RETURNING `+approverDeviceCols, newHash, now, deadHash))
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.ErrNotFound
		}
		if err != nil {
			return mapDBError(err)
		}
		if _, err := x.ExecContext(ctx, `UPDATE approver_pairings SET device_id = $1 WHERE code_hash = $2`, d.ID, codeHash); err != nil {
			return mapDBError(err)
		}
		out = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// TouchDevice implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) TouchDevice(ctx context.Context, id string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE approver_devices SET last_used_at = $1 WHERE id = $2`, now, id)
	return mapDBError(err)
}

// RevokeDevice implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) RevokeDevice(ctx context.Context, id string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE approver_devices SET revoked_at = $1, prev_token_hash = NULL, rotation_nonce = NULL, share_until = NULL
		WHERE id = $2 AND revoked_at IS NULL`, now, id)
	return mapDBError(err)
}

// CreateRequest implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) CreateRequest(ctx context.Context, q persistence.AgentApprovalRequestRow) error {
	return insertApprovalRequest(ctx, r.db, q)
}

const approvalRequestCols = `id, namespace, kind, sentence, rendered, rendered_sha256, status, created_at, expires_at, decided_at, decided_by_device, applied_at, apply_attempts, apply_error, decided_choice`

func scanApprovalRequest(s interface{ Scan(...interface{}) error }) (persistence.AgentApprovalRequestRow, error) {
	var (
		q                    persistence.AgentApprovalRequestRow
		rendered             string
		decided, applied     sql.NullTime
		by, applyErr, choice sql.NullString
	)
	if err := s.Scan(&q.ID, &q.Namespace, &q.Kind, &q.Sentence, &rendered, &q.RenderedSHA256, &q.Status,
		&q.CreatedAt, &q.ExpiresAt, &decided, &by, &applied, &q.ApplyAttempts, &applyErr, &choice); err != nil {
		return q, err
	}
	q.Rendered, q.DecidedByDevice, q.ApplyError, q.DecidedChoice = []byte(rendered), by.String, applyErr.String, choice.String
	if decided.Valid {
		t := decided.Time
		q.DecidedAt = &t
	}
	if applied.Valid {
		t := applied.Time
		q.AppliedAt = &t
	}
	return q, nil
}

// GetRequest implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) GetRequest(ctx context.Context, id string) (*persistence.AgentApprovalRequestRow, error) {
	q, err := scanApprovalRequest(r.db.QueryRowContext(ctx, `SELECT `+approvalRequestCols+` FROM agent_approval_requests WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return &q, nil
}

func (r *ApproverDeviceRepository) listRequests(ctx context.Context, where string, args ...interface{}) ([]persistence.AgentApprovalRequestRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+approvalRequestCols+` FROM agent_approval_requests WHERE `+where, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentApprovalRequestRow
	for rows.Next() {
		q, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, q)
	}
	return out, mapDBError(rows.Err())
}

// ListPending implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ListPending(ctx context.Context, now time.Time) ([]persistence.AgentApprovalRequestRow, error) {
	return r.listRequests(ctx, `status = 'pending' AND expires_at > $1 ORDER BY created_at DESC, id DESC`, now)
}

// Decide implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) Decide(ctx context.Context, id, shownSHA256, deviceID string, approve bool, now time.Time) error {
	return r.DecideWithChoice(ctx, id, shownSHA256, deviceID, approve, "", now)
}

// DecideWithChoice implements persistence.ApproverDeviceRepository. An empty
// choice stores NULL.
func (r *ApproverDeviceRepository) DecideWithChoice(ctx context.Context, id, shownSHA256, deviceID string, approve bool, choice string, now time.Time) error {
	status := persistence.ApprovalRejected
	if approve {
		status = persistence.ApprovalApproved
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE agent_approval_requests SET status = $1, decided_at = $2, decided_by_device = $3, decided_choice = $6
		WHERE id = $4 AND status = 'pending' AND rendered_sha256 = $5 AND expires_at > $2`,
		status, now, deviceID, id, shownSHA256, sql.NullString{String: choice, Valid: choice != ""})
	if err != nil {
		return mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", persistence.ErrApprovalNoTransition, id)
	}
	return nil
}

// ClaimApply implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ClaimApply(ctx context.Context, id, holder string, until, now time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE agent_approval_requests
		SET apply_holder = $1, apply_lease_until = $2, apply_attempts = apply_attempts + 1
		WHERE id = $3 AND status = 'approved' AND applied_at IS NULL
		  AND (apply_lease_until IS NULL OR apply_lease_until <= $4 OR apply_holder = $1)`,
		holder, until, id, now)
	if err != nil {
		return false, mapDBError(err)
	}
	n, err := res.RowsAffected()
	return n == 1, mapDBError(err)
}

// MarkApplied implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) MarkApplied(ctx context.Context, id string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_approval_requests SET applied_at = $1, apply_holder = NULL, apply_lease_until = NULL WHERE id = $2 AND applied_at IS NULL`, now, id)
	return mapDBError(err)
}

// MarkApplyFailed implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) MarkApplyFailed(ctx context.Context, id, reason string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_approval_requests SET applied_at = $1, apply_error = $2, apply_holder = NULL, apply_lease_until = NULL WHERE id = $3 AND applied_at IS NULL`,
		now, reason, id)
	return mapDBError(err)
}

// ListApprovedUnapplied implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ListApprovedUnapplied(ctx context.Context) ([]persistence.AgentApprovalRequestRow, error) {
	return r.listRequests(ctx, `status = 'approved' AND applied_at IS NULL ORDER BY decided_at, id`)
}

// ListRecentByNamespace implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ListRecentByNamespace(ctx context.Context, namespace string, since time.Time) ([]persistence.AgentApprovalRequestRow, error) {
	return r.listRequests(ctx, `namespace = $1 AND created_at >= $2 ORDER BY created_at DESC, id DESC`, namespace, since)
}

// ExpirePending implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ExpirePending(ctx context.Context, now time.Time) (int, error) {
	rows, err := r.ExpirePendingRows(ctx, now)
	return len(rows), err
}

// ExpirePendingRows implements persistence.ApproverDeviceRepository: one
// UPDATE ... RETURNING, so a row is reported by exactly the pass that
// expired it.
func (r *ApproverDeviceRepository) ExpirePendingRows(ctx context.Context, now time.Time) ([]persistence.AgentApprovalRequestRow, error) {
	rows, err := r.db.QueryContext(ctx, `UPDATE agent_approval_requests SET status = 'expired' WHERE status = 'pending' AND expires_at <= $1 RETURNING `+approvalRequestCols, now)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentApprovalRequestRow
	for rows.Next() {
		q, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, q)
	}
	return out, mapDBError(rows.Err())
}

// approvalCapLockPrefix keys the transaction advisory lock that serialises
// CreateRequestCapped's count with its insert, per namespace (the
// budget-reservation pattern; Hermes approval transport design §4.4).
const approvalCapLockPrefix = "agent_approval_requests:cap:"

// CreateRequestCapped implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) CreateRequestCapped(ctx context.Context, q persistence.AgentApprovalRequestRow, c persistence.ApprovalCap, now time.Time) error {
	return r.withTx(ctx, func(x persistence.DBTX) error {
		if _, err := x.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, approvalCapLockPrefix+q.Kind+":"+q.Namespace); err != nil {
			return mapDBError(err)
		}
		var pending, recent int
		if err := x.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM agent_approval_requests
			WHERE namespace = $1 AND kind = $2 AND status = 'pending' AND expires_at > $3`,
			q.Namespace, q.Kind, now).Scan(&pending); err != nil {
			return mapDBError(err)
		}
		if err := x.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM agent_approval_requests
			WHERE namespace = $1 AND kind = $2 AND created_at >= $3`,
			q.Namespace, q.Kind, c.Since).Scan(&recent); err != nil {
			return mapDBError(err)
		}
		if r.afterCount != nil {
			r.afterCount()
		}
		// ">=" and not ">": the counts exclude the row about to be inserted,
		// so a filing is refused when MaxPending already wait. SQLite inserts
		// first, counts including the new row, and uses ">" for the same
		// limit. The shared contract test (repotest approverCappedCreate)
		// refuses the MaxPending+1th filing and the MaxRecent+1th on both
		// drivers.
		switch {
		case pending >= c.MaxPending:
			return persistence.ErrApprovalCapPending
		case recent >= c.MaxRecent:
			return persistence.ErrApprovalCapRecent
		}
		return insertApprovalRequest(ctx, x, q)
	})
}

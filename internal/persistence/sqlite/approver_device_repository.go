package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ApproverDeviceRepository implements persistence.ApproverDeviceRepository
// over SQLite. It mirrors the Postgres implementation;
// repotest.RunApproverDeviceSuite keeps the two honest.
type ApproverDeviceRepository struct {
	db *sql.DB
	// afterCount runs inside RedeemPairing once the device count is read;
	// a test seam that makes the first-device race deterministic.
	afterCount func()
}

// NewApproverDeviceRepository wires the repository.
func NewApproverDeviceRepository(db *sql.DB) *ApproverDeviceRepository {
	return &ApproverDeviceRepository{db: db}
}

var _ persistence.ApproverDeviceRepository = (*ApproverDeviceRepository)(nil)

// SetRedeemHookForTest installs a function RedeemPairing calls right after it
// reads the device count. Production code never sets it.
func (r *ApproverDeviceRepository) SetRedeemHookForTest(fn func()) { r.afterCount = fn }

// CreatePairing implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) CreatePairing(ctx context.Context, p persistence.ApproverPairingRow) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO approver_pairings (code_hash, label, created_at, expires_at)
		VALUES (?, ?, ?, ?)`, p.CodeHash, p.Label, sqliteTime(p.CreatedAt), sqliteTime(p.ExpiresAt))
	return err
}

// GetPairing implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) GetPairing(ctx context.Context, codeHash string, now time.Time) (*persistence.ApproverPairingRow, error) {
	var (
		p                persistence.ApproverPairingRow
		created, expires sqlTime
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT code_hash, label, created_at, expires_at FROM approver_pairings
		WHERE code_hash = ? AND redeemed_at IS NULL AND expires_at > ?`, codeHash, sqliteTime(now)).
		Scan(&p.CodeHash, &p.Label, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.ExpiresAt = created.Time, expires.Time
	return &p, nil
}

// RedeemPairing implements persistence.ApproverDeviceRepository.
//
// Two things serialise it, and either alone suffices: the daemon's DSN begins
// read-write transactions IMMEDIATE (_txlock=immediate, sqlite.go), and the
// transaction's first statement is the UPDATE that consumes the code, which
// takes SQLite's single writer lock before the device count is read. A
// second redeemer therefore waits (busy_timeout) for the first to commit and
// counts the committed device. Keep the write first: it holds even on a
// handle opened without the DSN parameter (verified 2026-10-02 with the
// suite's deterministic race).
func (r *ApproverDeviceRepository) RedeemPairing(ctx context.Context, codeHash, claimHash string, d persistence.ApproverDeviceRow, req persistence.AgentApprovalRequestRow, now time.Time) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE approver_pairings SET redeemed_at = ?
		WHERE code_hash = ? AND redeemed_at IS NULL AND expires_at > ?`,
		sqliteTime(now), codeHash, sqliteTime(now))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, persistence.ErrNotFound
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_devices WHERE revoked_at IS NULL`).Scan(&active); err != nil {
		return false, err
	}
	if r.afterCount != nil {
		r.afterCount()
	}
	first := active == 0
	if first {
		if err := insertApproverDevice(ctx, tx, d); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approver_pairings SET device_id = ? WHERE code_hash = ?`, d.ID, codeHash); err != nil {
			return false, err
		}
	} else {
		if err := insertApprovalRequest(ctx, tx, req); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approver_pairings SET claim_hash = ?, request_id = ? WHERE code_hash = ?`,
			claimHash, req.ID, codeHash); err != nil {
			return false, err
		}
	}
	return first, tx.Commit()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func insertApproverDevice(ctx context.Context, x execer, d persistence.ApproverDeviceRow) error {
	_, err := x.ExecContext(ctx, `
		INSERT INTO approver_devices (id, label, token_hash, paired_at, paired_by, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		d.ID, d.Label, d.TokenHash, sqliteTime(d.PairedAt), d.PairedBy, sqliteTime(d.LastUsedAt))
	return err
}

func insertApprovalRequest(ctx context.Context, x execer, q persistence.AgentApprovalRequestRow) error {
	_, err := x.ExecContext(ctx, `
		INSERT INTO agent_approval_requests (id, namespace, kind, sentence, rendered, rendered_sha256, status, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.ID, q.Namespace, q.Kind, q.Sentence, string(q.Rendered), q.RenderedSHA256, q.Status,
		sqliteTime(q.CreatedAt), sqliteTime(q.ExpiresAt))
	return err
}

// GetPairingByClaim implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) GetPairingByClaim(ctx context.Context, claimHash string) (*persistence.ApproverPairingRow, error) {
	var (
		p                  persistence.ApproverPairingRow
		created, expires   sqlTime
		redeemed           sqlNullTime
		claim, req, device sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT code_hash, label, created_at, expires_at, redeemed_at, claim_hash, request_id, device_id
		FROM approver_pairings WHERE claim_hash = ?`, claimHash).
		Scan(&p.CodeHash, &p.Label, &created, &expires, &redeemed, &claim, &req, &device)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.ExpiresAt = created.Time, expires.Time
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
	tx, err := r.db.BeginTx(ctx, nil) // the guarded UPDATE is the first statement, as in RedeemPairing
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE approver_pairings SET device_id = ? WHERE claim_hash = ? AND device_id IS NULL`, d.ID, claimHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistence.ErrNotFound
	}
	if err := insertApproverDevice(ctx, tx, d); err != nil {
		return err
	}
	return tx.Commit()
}

// RemintEnrollmentToken implements persistence.ApproverDeviceRepository. One
// statement, so the guards and the swap are atomic.
func (r *ApproverDeviceRepository) RemintEnrollmentToken(ctx context.Context, claimHash, newHash string, notBefore time.Time) (*persistence.ApproverDeviceRow, error) {
	d, err := scanApproverDevice(r.db.QueryRowContext(ctx, `
		UPDATE approver_devices SET dead_token_hash = token_hash, dead_reason = 'confirmed', token_hash = ?
		WHERE id = (SELECT device_id FROM approver_pairings WHERE claim_hash = ? AND device_id IS NOT NULL)
		  AND revoked_at IS NULL AND paired_at >= ? AND last_used_at = paired_at
		  AND dead_token_hash IS NULL AND prev_token_hash IS NULL
		RETURNING `+approverDeviceCols, newHash, claimHash, sqliteTime(notBefore)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

const approverDeviceCols = `id, label, token_hash, paired_at, paired_by, last_used_at, revoked_at,
	prev_token_hash, rotation_nonce, share_until, dead_token_hash, dead_reason, share_admitted, share_streak`

func scanApproverDevice(s interface{ Scan(...interface{}) error }) (persistence.ApproverDeviceRow, error) {
	var (
		d                         persistence.ApproverDeviceRow
		paired, used              sqlTime
		revoked, shareUntil       sqlNullTime
		prev, nonce, dead, reason sql.NullString
		admitted                  int
	)
	if err := s.Scan(&d.ID, &d.Label, &d.TokenHash, &paired, &d.PairedBy, &used, &revoked,
		&prev, &nonce, &shareUntil, &dead, &reason, &admitted, &d.ShareStreak); err != nil {
		return d, err
	}
	d.PairedAt, d.LastUsedAt = paired.Time, used.Time
	if revoked.Valid {
		t := revoked.Time
		d.RevokedAt = &t
	}
	if shareUntil.Valid {
		t := shareUntil.Time
		d.ShareUntil = &t
	}
	d.PrevTokenHash, d.RotationNonce, d.DeadTokenHash, d.DeadReason = prev.String, nonce.String, dead.String, reason.String
	d.ShareAdmitted = admitted != 0
	return d, nil
}

// GetDeviceByTokenHash implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) GetDeviceByTokenHash(ctx context.Context, tokenHash string) (*persistence.ApproverDeviceRow, error) {
	d, err := scanApproverDevice(r.db.QueryRowContext(ctx, `SELECT `+approverDeviceCols+` FROM approver_devices
		WHERE token_hash = ?1 OR prev_token_hash = ?1 OR dead_token_hash = ?1 LIMIT 1`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDevices implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ListDevices(ctx context.Context) ([]persistence.ApproverDeviceRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+approverDeviceCols+` FROM approver_devices ORDER BY paired_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.ApproverDeviceRow
	for rows.Next() {
		d, err := scanApproverDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountActiveDevices implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) CountActiveDevices(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_devices WHERE revoked_at IS NULL`).Scan(&n)
	return n, err
}

// RotateToken implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) RotateToken(ctx context.Context, id, presentedHash, newHash, nonce string, shareUntil, now time.Time) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET
			dead_token_hash = CASE WHEN prev_token_hash IS NOT NULL THEN prev_token_hash ELSE dead_token_hash END,
			dead_reason = CASE WHEN prev_token_hash IS NOT NULL THEN 'confirmed' ELSE dead_reason END,
			share_streak = CASE WHEN share_admitted <> 0 THEN share_streak ELSE 0 END,
			share_admitted = 0,
			prev_token_hash = ?, token_hash = ?, rotation_nonce = ?, share_until = ?, last_used_at = ?
		WHERE id = ? AND token_hash = ? AND revoked_at IS NULL`,
		presentedHash, newHash, nonce, sqliteTime(shareUntil), sqliteTime(now), id, presentedHash)
	if err != nil {
		return err
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
		UPDATE approver_devices SET share_admitted = 1, share_streak = share_streak + 1
		WHERE id = ? AND rotation_nonce = ? AND share_admitted = 0
		RETURNING share_streak`, id, nonce).Scan(&streak)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return streak, true, nil
}

// ConfirmToken implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ConfirmToken(ctx context.Context, id, currentHash string) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET dead_token_hash = prev_token_hash, dead_reason = 'confirmed',
			prev_token_hash = NULL, rotation_nonce = NULL, share_until = NULL
		WHERE id = ? AND token_hash = ? AND prev_token_hash IS NOT NULL`, id, currentHash); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET dead_reason = 'confirmed'
		WHERE id = ? AND token_hash = ? AND prev_token_hash IS NULL AND dead_reason = 'expired'`, id, currentHash)
	return err
}

// CloseShare implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) CloseShare(ctx context.Context, id, nonce string, until time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET share_until = ?
		WHERE id = ? AND rotation_nonce = ? AND share_until > ?`, sqliteTime(until), id, nonce, sqliteTime(until))
	return err
}

// ExpireShares implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ExpireShares(ctx context.Context, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE approver_devices SET dead_token_hash = prev_token_hash, dead_reason = 'expired',
			prev_token_hash = NULL, rotation_nonce = NULL, share_until = NULL
		WHERE prev_token_hash IS NOT NULL AND share_until <= ?`, sqliteTime(now))
	return err
}

// ResumeDevice implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ResumeDevice(ctx context.Context, codeHash, deadHash, newHash string, now time.Time) (*persistence.ApproverDeviceRow, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE approver_pairings SET redeemed_at = ?
		WHERE code_hash = ? AND redeemed_at IS NULL AND expires_at > ?`,
		sqliteTime(now), codeHash, sqliteTime(now))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, persistence.ErrNotFound
	}
	d, err := scanApproverDevice(tx.QueryRowContext(ctx, `
		UPDATE approver_devices SET token_hash = ?, last_used_at = ?,
			prev_token_hash = NULL, rotation_nonce = NULL, share_until = NULL,
			dead_token_hash = NULL, dead_reason = NULL, share_admitted = 0, share_streak = 0
		WHERE dead_token_hash = ? AND dead_reason = 'expired' AND revoked_at IS NULL
		RETURNING `+approverDeviceCols, newHash, sqliteTime(now), deadHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approver_pairings SET device_id = ? WHERE code_hash = ?`, d.ID, codeHash); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &d, nil
}

// TouchDevice implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) TouchDevice(ctx context.Context, id string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE approver_devices SET last_used_at = ? WHERE id = ?`, sqliteTime(now), id)
	return err
}

// RevokeDevice implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) RevokeDevice(ctx context.Context, id string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE approver_devices SET revoked_at = ?, prev_token_hash = NULL, rotation_nonce = NULL, share_until = NULL
		WHERE id = ? AND revoked_at IS NULL`, sqliteTime(now), id)
	return err
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
		created, expires     sqlTime
		decided, applied     sqlNullTime
		by, applyErr, choice sql.NullString
	)
	if err := s.Scan(&q.ID, &q.Namespace, &q.Kind, &q.Sentence, &rendered, &q.RenderedSHA256, &q.Status,
		&created, &expires, &decided, &by, &applied, &q.ApplyAttempts, &applyErr, &choice); err != nil {
		return q, err
	}
	q.Rendered = []byte(rendered)
	q.CreatedAt, q.ExpiresAt, q.DecidedByDevice, q.ApplyError = created.Time, expires.Time, by.String, applyErr.String
	q.DecidedChoice = choice.String
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
	q, err := scanApprovalRequest(r.db.QueryRowContext(ctx, `SELECT `+approvalRequestCols+` FROM agent_approval_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

func (r *ApproverDeviceRepository) listRequests(ctx context.Context, where string, args ...interface{}) ([]persistence.AgentApprovalRequestRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+approvalRequestCols+` FROM agent_approval_requests WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentApprovalRequestRow
	for rows.Next() {
		q, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ListPending implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ListPending(ctx context.Context, now time.Time) ([]persistence.AgentApprovalRequestRow, error) {
	return r.listRequests(ctx, `status = 'pending' AND expires_at > ? ORDER BY created_at DESC, id DESC`, sqliteTime(now))
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
		UPDATE agent_approval_requests SET status = ?, decided_at = ?, decided_by_device = ?, decided_choice = ?
		WHERE id = ? AND status = 'pending' AND rendered_sha256 = ? AND expires_at > ?`,
		status, sqliteTime(now), deviceID, sql.NullString{String: choice, Valid: choice != ""}, id, shownSHA256, sqliteTime(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", persistence.ErrApprovalNoTransition, id)
	}
	return nil
}

// CreateRequestCapped implements persistence.ApproverDeviceRepository. The
// INSERT is the transaction's first statement, so it takes SQLite's writer
// lock before the counts are read (as RedeemPairing's code-consuming
// UPDATE does), and the counts include the row just inserted: over either
// cap the transaction rolls back and nothing is written.
func (r *ApproverDeviceRepository) CreateRequestCapped(ctx context.Context, q persistence.AgentApprovalRequestRow, c persistence.ApprovalCap, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertApprovalRequest(ctx, tx, q); err != nil {
		return err
	}
	var pending, recent int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agent_approval_requests
		WHERE namespace = ? AND kind = ? AND status = 'pending' AND expires_at > ?`,
		q.Namespace, q.Kind, sqliteTime(now)).Scan(&pending); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agent_approval_requests
		WHERE namespace = ? AND kind = ? AND created_at >= ?`,
		q.Namespace, q.Kind, sqliteTime(c.Since)).Scan(&recent); err != nil {
		return err
	}
	if r.afterCount != nil {
		r.afterCount()
	}
	// ">" and not ">=": the counts include the row just inserted, so a
	// filing is refused when it would make MaxPending+1. Postgres counts
	// before its insert and uses ">=" for the same limit. The shared
	// contract test (repotest approverCappedCreate) refuses the
	// MaxPending+1th filing and the MaxRecent+1th on both drivers.
	switch {
	case pending > c.MaxPending:
		return persistence.ErrApprovalCapPending
	case recent > c.MaxRecent:
		return persistence.ErrApprovalCapRecent
	}
	return tx.Commit()
}

// ClaimApply implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ClaimApply(ctx context.Context, id, holder string, until, now time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE agent_approval_requests
		SET apply_holder = ?, apply_lease_until = ?, apply_attempts = apply_attempts + 1
		WHERE id = ? AND status = 'approved' AND applied_at IS NULL
		  AND (apply_lease_until IS NULL OR apply_lease_until <= ? OR apply_holder = ?)`,
		holder, sqliteTime(until), id, sqliteTime(now), holder)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkApplied implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) MarkApplied(ctx context.Context, id string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_approval_requests SET applied_at = ?, apply_holder = NULL, apply_lease_until = NULL WHERE id = ? AND applied_at IS NULL`, sqliteTime(now), id)
	return err
}

// MarkApplyFailed implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) MarkApplyFailed(ctx context.Context, id, reason string, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE agent_approval_requests SET applied_at = ?, apply_error = ?, apply_holder = NULL, apply_lease_until = NULL WHERE id = ? AND applied_at IS NULL`,
		sqliteTime(now), reason, id)
	return err
}

// ListApprovedUnapplied implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ListApprovedUnapplied(ctx context.Context) ([]persistence.AgentApprovalRequestRow, error) {
	return r.listRequests(ctx, `status = 'approved' AND applied_at IS NULL ORDER BY decided_at, id`)
}

// ListRecentByNamespace implements persistence.ApproverDeviceRepository.
func (r *ApproverDeviceRepository) ListRecentByNamespace(ctx context.Context, namespace string, since time.Time) ([]persistence.AgentApprovalRequestRow, error) {
	return r.listRequests(ctx, `namespace = ? AND created_at >= ? ORDER BY created_at DESC, id DESC`, namespace, sqliteTime(since))
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
	rows, err := r.db.QueryContext(ctx, `UPDATE agent_approval_requests SET status = 'expired' WHERE status = 'pending' AND expires_at <= ? RETURNING `+approvalRequestCols, sqliteTime(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.AgentApprovalRequestRow
	for rows.Next() {
		q, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

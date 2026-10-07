package persistence

import (
	"context"
	"errors"
	"time"
)

// Approver devices, pairings and agent approval requests (agent-administered
// Vornik design §9; plan P2.1). Tokens and codes are stored only as sha256
// hex digests: this package never sees a raw device token or pairing code.

// ApproverDeviceRow is one paired phone browser.
type ApproverDeviceRow struct {
	ID         string // "dev_" + 16 hex
	Label      string
	TokenHash  string
	PairedAt   time.Time
	PairedBy   string // "first-device" | "device:<id>"
	LastUsedAt time.Time
	RevokedAt  *time.Time

	// Rotation sharing and dead-value recognition (agent-administered design
	// §9.2, amendment 2026-10-05 "Rotation must survive a lost response").
	// PrevTokenHash, RotationNonce and ShareUntil are set only while a
	// rotation is shared with in-flight requests; DeadTokenHash and DeadReason
	// name the most recently killed value. No plaintext value is stored.
	PrevTokenHash string
	RotationNonce string // 64 hex characters
	ShareUntil    *time.Time
	DeadTokenHash string
	DeadReason    string // DeadExpired | DeadConfirmed
	ShareAdmitted bool
	ShareStreak   int
}

// Why a device's previous value died (design §9.2, amendment 2026-10-05).
const (
	// DeadExpired: its share ended with the successor never presented — the
	// response that carried it was lost. Resumable with a pairing code.
	DeadExpired = "expired"
	// DeadConfirmed: the successor was presented, so the holder of this value
	// is not the browser that moved on. Not resumable.
	DeadConfirmed = "confirmed"
)

// ApproverPairingRow is one pairing code from `vornikctl pair-device`.
type ApproverPairingRow struct {
	CodeHash   string
	Label      string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RedeemedAt *time.Time
	ClaimHash  string // "" until redeemed on the pending path
	RequestID  string // the enrollment request, when one was needed
	DeviceID   string // set once a device exists for this pairing
}

// Approval request kinds and statuses.
const (
	ApprovalKindDeviceEnrollment = "device_enrollment"
	ApprovalKindWideningChange   = "widening_change"
	ApprovalKindCredentialSlot   = "credential_slot"
	// ApprovalKindBrokerAction approves one proposed write of an agent
	// project (agent-administered Vornik plan P4.8).
	ApprovalKindBrokerAction = "broker_action"
	// ApprovalKindHostAction answers an action on the agent's own machine,
	// enforced by the agent's host (Hermes approval transport design §4.1).
	// Its decision carries a scope (DecidedChoice).
	ApprovalKindHostAction = "host_action"
	// ApprovalKindMemoryRetention authorizes a specific idle chunk deletion.
	ApprovalKindMemoryRetention = "memory_retention"

	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
)

// ApprovalKinds is every kind the kind CHECK accepts (migration 212), for
// tests that must cover each one (the §18.7 describer enumeration).
var ApprovalKinds = []string{ApprovalKindDeviceEnrollment, ApprovalKindWideningChange,
	ApprovalKindCredentialSlot, ApprovalKindBrokerAction, ApprovalKindHostAction, ApprovalKindMemoryRetention}

// AgentApprovalRequestRow is one thing a device is asked to decide.
type AgentApprovalRequestRow struct {
	ID              string // "apr_" + 16 hex
	Namespace       string // "" for device enrollment
	Kind            string
	Sentence        string // template-rendered, never agent prose
	Rendered        []byte // canonical JSON of the exact change shown
	RenderedSHA256  string // over Rendered; decisions compare against this, never a re-serialisation
	Status          string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	DecidedAt       *time.Time
	DecidedByDevice string
	AppliedAt       *time.Time
	ApplyAttempts   int
	// ApplyError, with AppliedAt set, means the approved change could never
	// apply; that is the request's terminal state.
	ApplyError string
	// DecidedChoice is the scope a decision carried ("once", "session",
	// "deny" for a host_action); "" for every kind decided by a plain
	// approve or reject (Hermes approval transport design §4.2).
	DecidedChoice string
}

// ErrApprovalNoTransition means Decide matched no pending, unexpired row with
// the shown hash: already decided, expired, unknown, or changed after display.
var ErrApprovalNoTransition = errors.New("approval request: no transition")

// ApprovalCap bounds CreateRequestCapped (Hermes approval transport design
// §4.4): at most MaxPending pending, unexpired requests of the row's kind in
// its namespace, and at most MaxRecent of them created at or after Since.
type ApprovalCap struct {
	MaxPending int
	MaxRecent  int
	Since      time.Time
}

// The two refusals of CreateRequestCapped. Nothing is written on either.
var (
	ErrApprovalCapPending = errors.New("approval request: too many pending requests")
	ErrApprovalCapRecent  = errors.New("approval request: too many requests in the last hour")
)

// ApproverDeviceRepository persists devices, pairings and approval requests.
type ApproverDeviceRepository interface {
	CreatePairing(ctx context.Context, p ApproverPairingRow) error
	// GetPairing returns an unexpired, unredeemed pairing, or ErrNotFound for
	// a code that is unknown, expired or used (one miss, no detail).
	GetPairing(ctx context.Context, codeHash string, now time.Time) (*ApproverPairingRow, error)
	// RedeemPairing atomically consumes an unexpired, unredeemed code. When no
	// unrevoked device exists it inserts newDevice and returns first=true.
	// Otherwise it records claimHash, inserts pendingReq and sets the
	// pairing's request_id, all in the same transaction (plan amendment 6),
	// and returns first=false. The "no unrevoked device" test and the insert
	// are serialised: Postgres holds a transaction advisory lock; SQLite takes
	// its writer lock with the code-consuming UPDATE before counting. That
	// serialisation is itself under test (amendment 8). An unknown, expired or used code is ErrNotFound, with no
	// indication of which.
	RedeemPairing(ctx context.Context, codeHash, claimHash string, newDevice ApproverDeviceRow, pendingReq AgentApprovalRequestRow, now time.Time) (first bool, err error)
	// GetPairingByClaim returns ErrNotFound for an unknown claim.
	GetPairingByClaim(ctx context.Context, claimHash string) (*ApproverPairingRow, error)
	// CompletePairing inserts d and sets the pairing's device_id, once:
	// WHERE claim_hash=? AND device_id IS NULL. A second call is ErrNotFound.
	CompletePairing(ctx context.Context, claimHash string, d ApproverDeviceRow) error
	// RemintEnrollmentToken re-issues the token of the device a completed
	// pairing created, for a browser whose poll response was lost (design
	// §9.2, amendment 2026-10-07 T11). One guarded statement: the pairing
	// found by claimHash with device_id set; the device unrevoked; paired_at
	// >= notBefore; last_used_at = paired_at (never used); dead_token_hash and
	// prev_token_hash NULL (not re-minted, not rotated). It swaps token_hash
	// to newHash and records the old one as dead_token_hash with reason
	// DeadConfirmed. Any failed guard is ErrNotFound, with no detail.
	RemintEnrollmentToken(ctx context.Context, claimHash, newHash string, notBefore time.Time) (*ApproverDeviceRow, error)

	// GetDeviceByTokenHash finds the row whose token_hash, prev_token_hash or
	// dead_token_hash is tokenHash; the caller compares which matched. It
	// returns revoked rows too; the caller decides.
	GetDeviceByTokenHash(ctx context.Context, tokenHash string) (*ApproverDeviceRow, error)
	ListDevices(ctx context.Context) ([]ApproverDeviceRow, error)
	CountActiveDevices(ctx context.Context) (int, error)
	// RotateToken is a compare-and-swap on (id, token_hash = presentedHash,
	// not revoked); a miss is ErrNotFound. It opens a share: prev_token_hash =
	// presentedHash, the nonce, share_until. A share still open from an
	// earlier value ends with that value dead as DeadConfirmed. The streak
	// survives only if the previous share admitted a request; share_admitted
	// is consumed.
	RotateToken(ctx context.Context, id, presentedHash, newHash, nonce string, shareUntil, now time.Time) error
	// AdmitShare records that a request other than the rotating one was
	// admitted on the previous value during the share with this nonce. Only
	// the first admission of a share counts: it increments share_streak and
	// returns first=true with the new streak.
	AdmitShare(ctx context.Context, id, nonce string) (streak int, first bool, err error)
	// ConfirmToken runs when currentHash (the token_hash) is presented: an
	// open share ends with its previous value dead as DeadConfirmed, and a
	// DeadExpired predecessor is relabelled DeadConfirmed. Idempotent;
	// share_admitted and share_streak are left alone.
	ConfirmToken(ctx context.Context, id, currentHash string) error
	// CloseShare sets share_until = min(share_until, until) on the share with
	// this nonce; a stale nonce is a no-op.
	CloseShare(ctx context.Context, id, nonce string, until time.Time) error
	// ExpireShares ends every share whose share_until <= now, its previous
	// value dead as DeadExpired.
	ExpireShares(ctx context.Context, now time.Time) error
	// ResumeDevice, in one transaction, consumes an unexpired, unredeemed
	// pairing code and gives the unrevoked device whose dead value is
	// deadHash with reason DeadExpired a new token_hash, clearing its share,
	// dead and streak state. Either guard missing is ErrNotFound and nothing
	// changes.
	ResumeDevice(ctx context.Context, codeHash, deadHash, newHash string, now time.Time) (*ApproverDeviceRow, error)
	TouchDevice(ctx context.Context, id string, now time.Time) error
	// RevokeDevice is a no-op for an already revoked or unknown device. It
	// erases any open share (nonce included).
	RevokeDevice(ctx context.Context, id string, now time.Time) error

	CreateRequest(ctx context.Context, r AgentApprovalRequestRow) error
	// GetRequest returns ErrNotFound for an unknown id.
	GetRequest(ctx context.Context, id string) (*AgentApprovalRequestRow, error)
	// ListPending returns pending, unexpired requests, newest first.
	ListPending(ctx context.Context, now time.Time) ([]AgentApprovalRequestRow, error)
	// Decide moves a pending, unexpired request whose hash matches shownSHA256
	// to approved or rejected. Anything else is ErrApprovalNoTransition.
	Decide(ctx context.Context, id, shownSHA256, deviceID string, approve bool, now time.Time) error
	// DecideWithChoice is Decide that also writes decided_choice, in the same
	// guarded statement (Hermes approval transport design §4.2).
	DecideWithChoice(ctx context.Context, id, shownSHA256, deviceID string, approve bool, choice string, now time.Time) error
	// CreateRequestCapped inserts r unless the cap refuses it
	// (ErrApprovalCapPending, ErrApprovalCapRecent); the count and the insert
	// are serialised per namespace (Postgres: a transaction advisory lock;
	// SQLite: the insert comes first and takes the writer lock, and the count
	// includes it). A duplicate id is an error, never an overwrite.
	CreateRequestCapped(ctx context.Context, r AgentApprovalRequestRow, c ApprovalCap, now time.Time) error
	// ClaimApply leases an approved, unapplied request to holder until
	// until, counting the attempt. It succeeds only when no other holder's
	// lease is live, so in a cluster one node runs an effect at a time.
	// claimed=false is not an error.
	ClaimApply(ctx context.Context, id, holder string, until, now time.Time) (claimed bool, err error)
	// MarkApplied records the effect as run and releases the lease.
	MarkApplied(ctx context.Context, id string, now time.Time) error
	// MarkApplyFailed ends an approved request whose change can never apply,
	// with the reason; it is not retried.
	MarkApplyFailed(ctx context.Context, id, reason string, now time.Time) error
	// ListApprovedUnapplied returns approved requests whose effect has not run.
	ListApprovedUnapplied(ctx context.Context) ([]AgentApprovalRequestRow, error)
	// ListRecentByNamespace returns a namespace's requests created at or
	// after since, newest first (an exact namespace match).
	ListRecentByNamespace(ctx context.Context, namespace string, since time.Time) ([]AgentApprovalRequestRow, error)
	// ExpirePending moves pending requests past expires_at to expired.
	ExpirePending(ctx context.Context, now time.Time) (int, error)
	// ExpirePendingRows is ExpirePending that returns the rows it expired, as
	// expired, so a caller can count them by kind (Hermes approval transport
	// design §8).
	ExpirePendingRows(ctx context.Context, now time.Time) ([]AgentApprovalRequestRow, error)
}

package persistence

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Standing grants — https://docs.vornik.io
// 2026-09-29-broker-write-actions-and-push-design.md, "Tier 2: standing
// grants" as revised (rounds 3, 4) and settled by review 61a5.
//
// A grant lets later writes of one declared class (one project, workflow and
// action, one normalised key) be approved without a per-write decision, a
// bounded number of times before an expiry. Two statements are the whole of
// its trust:
//
//   - creation shares the seed action's pending→approved transaction, bound
//     to the hash the person was shown, under the per-project live bound;
//   - coverage is one guarded decrement (id AND key_hash AND live state AND
//     the action's own project/workflow/action), run only when the covered
//     action's own pending→approved affected one row, in the same
//     transaction.

// BrokerGrantApproverPrefix marks an action approved under a grant: the
// action's approver is "grant:<id>". It is the only approver string tier 2
// adds, and only BrokerGrantRepository.ApproveUnderGrant writes it.
const BrokerGrantApproverPrefix = "grant:"

// BrokerGrantApprover is the approver recorded on a covered action.
func BrokerGrantApprover(grantID string) string { return BrokerGrantApproverPrefix + grantID }

// BrokerGrantIDOfApprover returns the grant id of a covered action's
// approver, or "" for any other approver.
func BrokerGrantIDOfApprover(approver string) string {
	id, ok := strings.CutPrefix(approver, BrokerGrantApproverPrefix)
	if !ok {
		return ""
	}
	return id
}

// Errors.
var (
	// ErrBrokerGrantLimit: the project already has its maximum of live
	// grants. Nothing was written: not the grant, not the seed approval.
	ErrBrokerGrantLimit = errors.New("persistence: the project already has its maximum of live standing grants")
	// ErrBrokerGrantNotCovered: the guarded decrement affected no row (the
	// grant is revoked, paused, suspended, expired, used up, or its key or
	// class is not the action's). The action's approval was rolled back; it
	// is still pending.
	ErrBrokerGrantNotCovered = errors.New("persistence: the standing grant does not cover this action")
	// ErrBrokerGrantNoTransition: a pause, unpause, revoke, suspend or
	// confirm found the grant not in a state it applies to.
	ErrBrokerGrantNoTransition = errors.New("persistence: standing grant not in a valid state for this change")
)

// BrokerStandingGrant is one grant row (tier 2 revised, item 10).
type BrokerStandingGrant struct {
	ID         string
	ProjectID  string
	Namespace  string // the agent namespace, "" for an operator project
	WorkflowID string
	Action     string
	// KeyPaths are the proposal's standing.key at creation.
	KeyPaths []string
	// KeyValuesSealed is the normalised key's canonical JSON, sealed with the
	// secret-store seal. Opened only by the matcher and the grant pages.
	KeyValuesSealed string
	// KeyHash is SHA-256 of the canonical key, domain-separated for numeric
	// values (brokergrants.KeyOf); the
	// guarded decrement compares it, never the values.
	KeyHash             string
	MaxUses             int
	UsesLeft            int
	ExpiresAt           time.Time
	CreatedAt           time.Time
	CreatedBy           string // "device:<id>" or the operator
	SeedActionID        string
	ReachHashAtCreation string
	Active              bool // false once revoked: final
	Paused              bool
	SuspendedAt         *time.Time
	RevokedAt           *time.Time
	// DigestThrough: covered actions decided up to here were counted in a
	// digest already.
	DigestThrough time.Time
}

// Live reports whether the grant counts toward the project bound: not
// revoked, not expired, uses left. A paused or suspended grant is live (it
// can be resumed).
func (g *BrokerStandingGrant) Live(now time.Time) bool {
	return g != nil && g.Active && g.UsesLeft > 0 && g.ExpiresAt.After(now)
}

// BrokerGrantCount is one project's row of the live and paused gauges.
type BrokerGrantCount struct {
	ProjectID string
	Live      int64
	Paused    int64
}

// BrokerGrantAllAgentNamespaces as a filter's Namespace selects every agent
// namespace's grants (the approver device's page).
const BrokerGrantAllAgentNamespaces = "*"

// BrokerGrantFilter selects grants for a page: one agent namespace, every
// agent namespace (BrokerGrantAllAgentNamespaces), or operator projects
// (Namespace "") optionally narrowed to ProjectIDs.
type BrokerGrantFilter struct {
	Namespace  string
	ProjectIDs []string
	Limit      int
}

// BrokerGrantRepository persists standing grants.
type BrokerGrantRepository interface {
	// ApproveSeedAndCreate approves the seed action (pending→approved, bound
	// to the shown hash and its expiry) and inserts g in ONE transaction.
	// On Postgres the transaction first takes
	// pg_advisory_xact_lock(hashtext('broker_grants:' || project_id)); on
	// SQLite it is an immediate transaction. With maxLive live grants
	// already in the project it is ErrBrokerGrantLimit; a stale hash is
	// ErrBrokerActionNoTransition. Either way nothing is written.
	ApproveSeedAndCreate(ctx context.Context, actionID, shownArgsSHA256, approver string, g *BrokerStandingGrant, maxLive int, now time.Time) error
	// ApproveUnderGrant is the only path to an approval under a grant: the
	// action's pending→approved (approver grant:<id>, bound to argsSHA256 and
	// expiry), and, only when that affected one row, the guarded decrement
	// keyed on id AND key_hash AND the live state AND the action's own
	// project, workflow and action, in the same transaction. A decrement
	// that affects no row rolls the approval back: ErrBrokerGrantNotCovered.
	ApproveUnderGrant(ctx context.Context, actionID, argsSHA256, grantID, keyHash string, now time.Time) error

	Get(ctx context.Context, id string) (*BrokerStandingGrant, error)
	// ListForAction lists the grants of one class, any state, sooner
	// expiry first: the matcher's candidates.
	ListForAction(ctx context.Context, projectID, workflowID, action string) ([]*BrokerStandingGrant, error)
	// List lists grants for a page, newest first.
	List(ctx context.Context, f BrokerGrantFilter) ([]*BrokerStandingGrant, error)

	// SetPaused pauses (or unpauses) an active grant.
	SetPaused(ctx context.Context, id string, paused bool) error
	// Revoke ends an active grant; final.
	Revoke(ctx context.Context, id string, now time.Time) error
	// Suspend marks an active, unsuspended grant suspended; false when it
	// already was (or is not active).
	Suspend(ctx context.Context, id string, now time.Time) (bool, error)
	// Confirm clears a suspension and re-pins the grant's reach hash.
	Confirm(ctx context.Context, id, reachHash string) error

	// CountLive feeds the live and paused gauges, by project.
	CountLive(ctx context.Context, now time.Time) ([]BrokerGrantCount, error)
	// CoveredActions lists the actions approved under the grant, newest
	// first.
	CoveredActions(ctx context.Context, grantID string, limit int) ([]*BrokerAction, error)
	// DigestDue lists grants whose digest_through is at or before before.
	DigestDue(ctx context.Context, before time.Time) ([]*BrokerStandingGrant, error)
	// AdvanceDigest moves digest_through from from to to (a compare-and-set,
	// so two nodes never count one window twice) and returns how many
	// actions were approved under the grant in (from, to]. ok is false when
	// another pass advanced it first; the count is then 0.
	AdvanceDigest(ctx context.Context, id string, from, to time.Time) (n int, ok bool, err error)
}

package persistence

import (
	"context"
	"errors"
	"time"
)

// ErrAccountTransition refuses a busy or stale account decision (§22.2.16).
var ErrAccountTransition = errors.New("persistence: account transition is busy or stale")

// AccountDaemonScope is the generation shared by daemon-owned credentials.
const AccountDaemonScope = "daemon"

// AccountNamespaceScope returns the generation scope of an agent namespace.
func AccountNamespaceScope(ns string) string { return "ns:" + ns }

// AccountEpoch records one scope's monotonic credential generation.
type AccountEpoch struct {
	Scope string
	Epoch int64
}

// AccountDecisionGuard binds a decision to the namespace and daemon generations.
type AccountDecisionGuard []AccountEpoch

// AccountTransitionTicket identifies the owner and generation of a busy transition.
type AccountTransitionTicket struct {
	Scope string
	Epoch int64
	Owner string
}

// AccountPublication contains only prepared rows and named SQL operations.
// Agent credential rows are sealed. Non-agent OAuth rows preserve their existing
// operator storage representation; no operator encryption migration is implied.
type AccountPublication struct {
	Secret      *AgentSecretRow
	Token       *MCPOAuthToken
	DeleteToken *AccountTokenKey
	// DeleteTokens are the stored sign-ins of removed connections, deleted
	// in the same transaction as their Removed rows (GitHub #571 U3b,
	// mcp-server-authentication-design.md §15.3a C5).
	DeleteTokens []AccountTokenKey
	Identity     *AccountIdentityUpdate
	Integrations []AgentIntegrationApproval
	Removed      []AccountIntegrationRemoval
	// Namespace config completion validates the daemon epoch captured BEFORE apply.
	ExpectedDaemon *AccountEpoch
}

// AccountTokenKey identifies the OAuth token row to remove at publication.
type AccountTokenKey struct{ ProjectID, ServerName string }

// AccountIdentityUpdate records connection identity metadata at publication.
type AccountIdentityUpdate struct{ ProjectID, Integration, Identity string }

// AccountIntegrationRemoval marks an integration removed in the publication transaction.
type AccountIntegrationRemoval struct {
	ProjectID, Integration string
	At                     time.Time
}

// AccountTransitionRepository serializes decisions and atomic account publication.
type AccountTransitionRepository interface {
	ReadDecisionEpoch(context.Context, string) (AccountDecisionGuard, error)
	Begin(context.Context, string, string, string, time.Time) (AccountTransitionTicket, error)
	BeginConfig(context.Context, string, string, string, time.Time, AccountDecisionGuard) (AccountTransitionTicket, error)
	Finish(context.Context, AccountTransitionTicket, AccountPublication) error
	Abort(context.Context, AccountTransitionTicket) error
}

// AccountGuardedGrants exposes terminal transactions with explicit guards.
type AccountGuardedGrants interface {
	ConfirmGuarded(context.Context, string, string, AccountDecisionGuard) error
	ConfirmAccountGuarded(context.Context, string, string, string, string, string, string, AccountDecisionGuard) error
}

// AccountTransitionRow is one account-transition row as stored (design 22
// §22.2.16b, GitHub #669 item 3).
type AccountTransitionRow struct {
	Scope     string
	Epoch     int64
	Busy      bool
	Owner     string
	Operation string
	StartedAt string
}

// AccountTransitionRecovery lists busy rows and runs the controlled-recovery
// CAS (design 22, "Crash, error, abort and controlled recovery", automated by
// §22.2.16b). Both drivers implement it.
type AccountTransitionRecovery interface {
	// ListBusy returns the busy rows, by scope, and how many rows it examined.
	ListBusy(ctx context.Context) (busy []AccountTransitionRow, examined int, err error)
	// Recover clears one busy row in one transaction: epoch+1, busy false,
	// owner and operation empty, WHERE scope, epoch, owner and busy match.
	// It returns the row's before-values; when the CAS matches no row it
	// changes nothing and returns ErrAccountTransition.
	Recover(ctx context.Context, scope string, epoch int64, owner string) (AccountTransitionRow, error)
}

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
	Secret       *AgentSecretRow
	Token        *MCPOAuthToken
	DeleteToken  *AccountTokenKey
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

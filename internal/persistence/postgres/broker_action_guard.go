package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
)

// Lookup is advisory; the terminal predicate matches the exact stored project/vector.
func guardedPendingAction(ctx context.Context, x persistence.DBTX, id string) (*persistence.BrokerAction, error) {
	a, err := scanBrokerAction(x.QueryRowContext(ctx, `SELECT `+brokerActionColumns+` FROM broker_actions WHERE action_id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrBrokerActionNoTransition
	}
	if err != nil {
		return nil, err
	}
	if a.Status != persistence.BrokerActionPending {
		return nil, persistence.ErrBrokerActionNoTransition
	}
	if ns, agent := agentns.FromID(a.ProjectID); agent {
		if err = checkAccountGuard(ctx, x, ns, a.AccountGuard); err != nil {
			return nil, err
		}
	}
	return a, nil
}
func approveGuardedAction(ctx context.Context, x persistence.DBTX, a *persistence.BrokerAction, hash, approver string, now time.Time) error {
	var nsEpoch, daemonEpoch any
	if _, agent := agentns.FromID(a.ProjectID); agent {
		if len(a.AccountGuard) != 2 {
			return persistence.ErrAccountTransition
		}
		daemonEpoch = a.AccountGuard[0].Epoch
		nsEpoch = a.AccountGuard[1].Epoch
	}
	return brokerTransitionErr(x.ExecContext(ctx, `UPDATE broker_actions SET status='approved', approver=$1, decided_at=$2 WHERE action_id=$3 AND status='pending' AND args_sha256=$4 AND expires_at>$5 AND project_id=$6 AND ((account_ns_epoch=$7 AND account_daemon_epoch=$8) OR (account_ns_epoch IS NULL AND account_daemon_epoch IS NULL AND $9))`, approver, now, a.ActionID, hash, now, a.ProjectID, nsEpoch, daemonEpoch, nsEpoch == nil))
}

package sqlite

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
	a, err := scanBrokerAction(x.QueryRowContext(ctx, `SELECT `+brokerActionColumns+` FROM broker_actions WHERE action_id=?`, id))
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
	return transitionErr(x.ExecContext(ctx, `UPDATE broker_actions SET status='approved', approver=?, decided_at=? WHERE action_id=? AND status='pending' AND args_sha256=? AND expires_at>? AND project_id=? AND ((account_ns_epoch=? AND account_daemon_epoch=?) OR (account_ns_epoch IS NULL AND account_daemon_epoch IS NULL AND ?))`, approver, sqliteTime(now), a.ActionID, hash, sqliteTime(now), a.ProjectID, nsEpoch, daemonEpoch, nsEpoch == nil))
}

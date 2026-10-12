package postgres

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"vornik.io/vornik/internal/persistence"
)

// AccountTransitionRepository implements durable account generations on postgres.
type AccountTransitionRepository struct{ db persistence.DBTX }

// NewAccountTransitionRepository wires the transaction-capable repository.
func NewAccountTransitionRepository(db persistence.DBTX) *AccountTransitionRepository {
	return &AccountTransitionRepository{db: db}
}

var _ persistence.AccountTransitionRepository = (*AccountTransitionRepository)(nil)

func (r *AccountTransitionRepository) transaction(ctx context.Context, fn func(persistence.DBTX) error) error {
	tx, owns, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return err
	}
	if !owns {
		return fn(r.db)
	}
	defer func() { _ = tx.Rollback() }()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type accountState struct {
	epoch int64
	busy  bool
	owner string
}

// Every multi-row operation sorts by the SAME total lexical scope-key order.
// daemon precedes ns: solely because those key prefixes sort that way.
//
// A scope in exclusive is locked FOR UPDATE (the transaction advances or
// clears it); every other scope FOR SHARE (it only reads it). FOR SHARE
// conflicts with FOR UPDATE, so a decision and a transition on one row still
// order against each other, while decisions in different namespaces no
// longer serialise on the shared daemon row (design §22.2.16, change
// 2026-10-12, audit finding D). No caller locks one row shared and later
// exclusive in the same transaction: that upgrade could deadlock.
func lockAccountStates(ctx context.Context, x persistence.DBTX, scopes []string, exclusive ...string) (map[string]accountState, error) {
	scopes = append([]string(nil), scopes...)
	sort.Strings(scopes)
	out := make(map[string]accountState, len(scopes))
	for _, scope := range scopes {
		if _, ok := out[scope]; ok {
			continue
		}
		if _, err := x.ExecContext(ctx, `INSERT INTO agent_account_transitions(scope) VALUES ($1) ON CONFLICT(scope) DO NOTHING`, scope); err != nil {
			return nil, err
		}
		mode := "FOR SHARE"
		if slices.Contains(exclusive, scope) {
			mode = "FOR UPDATE"
		}
		var s accountState
		if err := x.QueryRowContext(ctx, `SELECT epoch,busy,owner FROM agent_account_transitions WHERE scope = $1 `+mode, scope).Scan(&s.epoch, &s.busy, &s.owner); err != nil {
			return nil, err
		}
		out[scope] = s
	}
	return out, nil
}

// checkAccountGuard validates a decision's captured epochs; exclusive names
// the scope the caller goes on to advance in the same transaction.
func checkAccountGuard(ctx context.Context, x persistence.DBTX, ns string, guard persistence.AccountDecisionGuard, exclusive ...string) error {
	if ns == "" {
		return nil
	}
	if len(guard) != 2 {
		return persistence.ErrAccountTransition
	}
	expected := map[string]int64{}
	for _, e := range guard {
		expected[e.Scope] = e.Epoch
	}
	if len(expected) != 2 {
		return persistence.ErrAccountTransition
	}
	scopes := []string{persistence.AccountDaemonScope, persistence.AccountNamespaceScope(ns)}
	states, err := lockAccountStates(ctx, x, scopes, exclusive...)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		epoch, ok := expected[scope]
		s := states[scope]
		if !ok || s.busy || s.epoch != epoch {
			return persistence.ErrAccountTransition
		}
	}
	return nil
}

// ReadDecisionEpoch reads both steady generations under canonical scope locks.
func (r *AccountTransitionRepository) ReadDecisionEpoch(ctx context.Context, ns string) (out persistence.AccountDecisionGuard, err error) {
	err = r.transaction(ctx, func(x persistence.DBTX) error {
		scopes := []string{persistence.AccountDaemonScope, persistence.AccountNamespaceScope(ns)}
		states, e := lockAccountStates(ctx, x, scopes)
		if e != nil {
			return e
		}
		for _, scope := range scopes {
			s := states[scope]
			if s.busy {
				return persistence.ErrAccountTransition
			}
			out = append(out, persistence.AccountEpoch{Scope: scope, Epoch: s.epoch})
		}
		return nil
	})
	return
}

// Begin advances the scope generation and records its busy owner.
func (r *AccountTransitionRepository) Begin(ctx context.Context, scope, owner, operation string, now time.Time) (ticket persistence.AccountTransitionTicket, err error) {
	if scope == "" || owner == "" {
		return ticket, persistence.ErrAccountTransition
	}
	err = r.transaction(ctx, func(x persistence.DBTX) error {
		var e error
		ticket, e = beginAccountState(ctx, x, scope, owner, operation, now)
		return e
	})
	return
}
func beginAccountState(ctx context.Context, x persistence.DBTX, scope, owner, operation string, now time.Time) (ticket persistence.AccountTransitionTicket, err error) {
	states, e := lockAccountStates(ctx, x, []string{scope}, scope)
	if e != nil {
		return ticket, e
	}
	s := states[scope]
	if s.busy || s.epoch == math.MaxInt64 {
		return ticket, persistence.ErrAccountTransition
	}
	_, e = x.ExecContext(ctx, `UPDATE agent_account_transitions SET epoch=$1,busy=TRUE,owner=$2,operation=$3,started_at=$4 WHERE scope=$5`, s.epoch+1, owner, operation, now.UTC().Format(time.RFC3339Nano), scope)
	if e == nil {
		ticket = persistence.AccountTransitionTicket{Scope: scope, Epoch: s.epoch + 1, Owner: owner}
	}
	return ticket, e
}

// Finish atomically publishes prepared rows and clears the validated busy owner.
func (r *AccountTransitionRepository) Finish(ctx context.Context, t persistence.AccountTransitionTicket, p persistence.AccountPublication) error {
	return r.transaction(ctx, func(x persistence.DBTX) error {
		scopes := []string{t.Scope}
		if p.ExpectedDaemon != nil {
			if p.ExpectedDaemon.Scope != persistence.AccountDaemonScope {
				return persistence.ErrAccountTransition
			}
			scopes = append(scopes, persistence.AccountDaemonScope)
		}
		states, err := lockAccountStates(ctx, x, scopes, t.Scope)
		if err != nil {
			return err
		}
		s := states[t.Scope]
		if !s.busy || s.epoch != t.Epoch || s.owner != t.Owner {
			return persistence.ErrAccountTransition
		}
		if p.ExpectedDaemon != nil {
			d := states[persistence.AccountDaemonScope]
			if d.busy || d.epoch != p.ExpectedDaemon.Epoch {
				return persistence.ErrAccountTransition
			}
		}
		if err = publishAccountRows(ctx, x, p); err != nil {
			return err
		}
		_, err = x.ExecContext(ctx, `UPDATE agent_account_transitions SET busy=FALSE,owner='',operation='' WHERE scope=$1`, t.Scope)
		return err
	})
}

// Abort is valid only when the service has proved no publication/verified rollback.
// It never reinstates grants or rewinds the incremented epoch.
func (r *AccountTransitionRepository) Abort(ctx context.Context, t persistence.AccountTransitionTicket) error {
	return r.Finish(ctx, t, persistence.AccountPublication{})
}

// publishAccountRows contains only named transaction-bound SQL operations.
func publishAccountRows(ctx context.Context, x persistence.DBTX, p persistence.AccountPublication) error {
	var err error
	if p.Secret != nil {
		if err = NewAgentSecretRepository(x).Upsert(ctx, *p.Secret); err != nil {
			return err
		}
	}
	if p.Token != nil {
		if err = NewMCPOAuthTokenRepository(x).Upsert(ctx, p.Token); err != nil {
			return err
		}
	}
	if p.DeleteToken != nil {
		if err = NewMCPOAuthTokenRepository(x).Delete(ctx, p.DeleteToken.ProjectID, p.DeleteToken.ServerName); err != nil {
			return err
		}
	}
	grants := NewAgentGrantRepository(x)
	if p.Identity != nil {
		if err = grants.SetIntegrationIdentity(ctx, p.Identity.ProjectID, p.Identity.Integration, p.Identity.Identity); err != nil {
			return err
		}
	}
	for _, row := range p.Integrations {
		if err = grants.UpsertIntegration(ctx, row); err != nil {
			return err
		}
	}
	for _, row := range p.Removed {
		if err = grants.MarkIntegrationRemoved(ctx, row.ProjectID, row.Integration, row.At); err != nil {
			return err
		}
	}
	return nil
}

// BeginConfig validates both pre-snapshot epochs before namespace publication.
func (r *AccountTransitionRepository) BeginConfig(ctx context.Context, scope, owner, operation string, now time.Time, guard persistence.AccountDecisionGuard) (ticket persistence.AccountTransitionTicket, err error) {
	if !strings.HasPrefix(scope, "ns:") || scope == "ns:" || owner == "" {
		return ticket, persistence.ErrAccountTransition
	}
	ns := strings.TrimPrefix(scope, "ns:")
	err = r.transaction(ctx, func(x persistence.DBTX) error {
		if e := checkAccountGuard(ctx, x, ns, guard, scope); e != nil {
			return e
		}
		var e error
		ticket, e = beginAccountState(ctx, x, scope, owner, operation, now)
		return e
	})
	return
}

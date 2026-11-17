package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/persistence"
)

// The controlled recovery of a busy account-transition row whose holder is
// gone (design 22 §22.2.16b, GitHub #669 item 3): what an operator did by
// hand in SQL, with two more refusals. The order is the design's: the
// held-ticket check (a fast refusal), then the process's config-write lock,
// then the apply-journal check under the lock, then the reviewed CAS.

// accountRecovery adapts the container to api.AccountTransitionRecoverer.
type accountRecovery struct{ c *Container }

var _ api.AccountTransitionRecoverer = accountRecovery{}

func (a accountRecovery) repo() (persistence.AccountTransitionRecovery, *persistence.HeldTransitions, error) {
	if a.c == nil || a.c.repos == nil || a.c.repos.AccountTransitions == nil {
		return nil, nil, persistence.ErrTransitionRecoveryUnsupported
	}
	held, _ := a.c.repos.AccountTransitions.(*persistence.HeldTransitions)
	r, ok := a.c.repos.AccountTransitions.(persistence.AccountTransitionRecovery)
	if !ok {
		return nil, nil, persistence.ErrTransitionRecoveryUnsupported
	}
	return r, held, nil
}

// ListBusy lists the busy rows, with whether this process holds each.
func (a accountRecovery) ListBusy(ctx context.Context) ([]api.AccountTransitionView, int, error) {
	r, held, err := a.repo()
	if err != nil {
		return nil, 0, err
	}
	rows, examined, err := r.ListBusy(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]api.AccountTransitionView, 0, len(rows))
	for _, row := range rows {
		v := api.AccountTransitionView{Scope: row.Scope, Epoch: row.Epoch, Owner: row.Owner, Operation: row.Operation, StartedAt: row.StartedAt}
		if t, err := time.Parse(time.RFC3339Nano, row.StartedAt); err == nil {
			v.Age = time.Since(t).Round(time.Second)
		}
		v.HeldHere = held != nil && held.Holds(row.Owner)
		out = append(out, v)
	}
	return out, examined, nil
}

// Recover runs the recovery, refusing a ticket held here and an open
// apply-journal row that could touch the scope.
func (a accountRecovery) Recover(ctx context.Context, scope string, epoch int64, owner string) (persistence.AccountTransitionRow, error) {
	r, held, err := a.repo()
	if err != nil {
		return persistence.AccountTransitionRow{}, err
	}
	if held != nil && held.Holds(owner) {
		return persistence.AccountTransitionRow{}, api.ErrTransitionHeldHere
	}
	unlock := configlock.Lock()
	defer unlock()
	if err := a.journalClear(ctx, scope); err != nil {
		return persistence.AccountTransitionRow{}, err
	}
	return r.Recover(ctx, scope, epoch, owner)
}

// journalClear refuses while an open apply-journal row could touch the
// scope: for a namespace scope, a row affecting any project of that
// namespace; for the daemon scope, which spans every namespace, any open row.
// It runs under the config-write lock, which every journal writer takes
// (the #584 census), so no row opens between this check and the CAS.
func (a accountRecovery) journalClear(ctx context.Context, scope string) error {
	if a.c.repos.ApplyJournal == nil {
		return nil
	}
	rows, err := a.c.repos.ApplyJournal.ListOpen(ctx)
	if err != nil {
		return fmt.Errorf("account recovery: cannot read the apply journal: %w", err)
	}
	ns, isNS := strings.CutPrefix(scope, "ns:")
	var blocking []string
	for _, row := range rows {
		if !isNS {
			blocking = append(blocking, row.ID)
			continue
		}
		for _, p := range row.AffectedProjects {
			if got, agent := agentns.FromID(p); agent && got == ns {
				blocking = append(blocking, row.ID)
				break
			}
		}
	}
	if len(blocking) > 0 {
		return &api.JournalOpenError{IDs: blocking}
	}
	return nil
}

package repotest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
	"vornik.io/vornik/internal/persistence"
)

// RunQueuedActionSuite pins the account-binding regression on BOTH drivers.
func RunQueuedActionSuite(t *testing.T, actions persistence.BrokerActionRepository, grants persistence.BrokerGrantRepository, epochs persistence.AccountTransitionRepository) {
	s := queuedActionSuite{ctx: context.Background(), now: time.Now().UTC().Truncate(time.Microsecond), actions: actions, grants: grants, epochs: epochs}
	for _, scopeKind := range []string{"namespace", "daemon"} {
		for _, terminal := range []string{"ordinary", "standing", "seed"} {
			t.Run(scopeKind+"/"+terminal, func(t *testing.T) { s.staleTerminal(t, scopeKind, terminal) })
		}
	}
	for _, scope := range []string{persistence.AccountDaemonScope, "ns:queue"} {
		t.Run("busy Stage/"+scope, func(t *testing.T) { s.busyStage(t, scope) })
	}
	t.Run("replacement and pending immutability", s.replacement)
	t.Run("project collision", s.collision)
	t.Run("operator and reverse order", s.operator)
}

type queuedActionSuite struct {
	ctx     context.Context
	now     time.Time
	actions persistence.BrokerActionRepository
	grants  persistence.BrokerGrantRepository
	epochs  persistence.AccountTransitionRepository
}

func (s queuedActionSuite) stage(t *testing.T) *persistence.BrokerAction {
	t.Helper()
	a := &persistence.BrokerAction{ProjectID: "queue--mail", TaskID: uniqueID("queued-task"), WorkflowID: "reply", ActionKind: "send", Tool: "mcp__mail__send", Status: persistence.BrokerActionStaged, ArgsJSON: []byte(`{}`), ArgsSHA256: "hash", ExpiresAt: s.now.Add(time.Hour)}
	if _, err := s.actions.Stage(s.ctx, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func (s queuedActionSuite) promote(t *testing.T, a *persistence.BrokerAction) {
	t.Helper()
	if _, err := s.actions.PromoteStaged(s.ctx, a.TaskID); err != nil {
		t.Fatal(err)
	}
}

func (s queuedActionSuite) rotate(t *testing.T, scope string) {
	t.Helper()
	ticket, err := s.epochs.Begin(s.ctx, scope, uniqueID("owner"), "rotation", s.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.epochs.Abort(s.ctx, ticket); err != nil {
		t.Fatal(err)
	}
}

func (s queuedActionSuite) staleTerminal(t *testing.T, scopeKind, terminal string) {
	ns := "queue"
	a := s.stage(t)
	s.promote(t, a)
	vector, err := s.epochs.ReadDecisionEpoch(s.ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	g := &persistence.BrokerStandingGrant{ID: uniqueID("queued-grant"), ProjectID: a.ProjectID, Namespace: ns, WorkflowID: a.WorkflowID, Action: a.ActionKind, KeyPaths: []string{}, KeyValuesSealed: "sealed", KeyHash: "key", MaxUses: 5, UsesLeft: 5, CreatedAt: s.now, ExpiresAt: s.now.Add(time.Hour), DigestThrough: s.now, DecisionGuard: vector}
	if terminal == "standing" {
		seed := s.stage(t)
		s.promote(t, seed)
		g.SeedActionID = seed.ActionID
		if err = s.grants.ApproveSeedAndCreate(s.ctx, seed.ActionID, seed.ArgsSHA256, "device", g, 1000, s.now); err != nil {
			t.Fatal(err)
		}
	}
	scope := persistence.AccountNamespaceScope(ns)
	if scopeKind == "daemon" {
		scope = persistence.AccountDaemonScope
	}
	s.rotate(t, scope)
	switch terminal {
	case "ordinary":
		err = s.actions.Approve(s.ctx, a.ActionID, a.ArgsSHA256, "device", s.now)
	case "standing":
		err = s.grants.ApproveUnderGrant(s.ctx, a.ActionID, a.ArgsSHA256, g.ID, g.KeyHash, s.now)
	case "seed":
		g.SeedActionID = a.ActionID
		g.DecisionGuard, err = s.epochs.ReadDecisionEpoch(s.ctx, ns)
		if err != nil {
			t.Fatal(err)
		}
		err = s.grants.ApproveSeedAndCreate(s.ctx, a.ActionID, a.ArgsSHA256, "device", g, 1000, s.now)
	}
	if !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("stale %s terminal=%v", terminal, err)
	}
	got, err := s.actions.Get(s.ctx, a.ActionID)
	if err != nil || got.Status != persistence.BrokerActionPending {
		t.Fatalf("partial action terminal: %+v %v", got, err)
	}
	if terminal == "standing" {
		got, err := s.grants.Get(s.ctx, g.ID)
		if err != nil || got.UsesLeft != 5 {
			t.Fatalf("partial decrement: %+v %v", got, err)
		}
	}
	if terminal == "seed" {
		if _, err := s.grants.Get(s.ctx, g.ID); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("partial seed: %v", err)
		}
	}
}

func (s queuedActionSuite) busyStage(t *testing.T, scope string) {
	ticket, err := s.epochs.Begin(s.ctx, scope, uniqueID("owner"), "rotate", s.now)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := s.epochs.Abort(s.ctx, ticket); e != nil {
			t.Error(e)
		}
	}()
	a := &persistence.BrokerAction{ProjectID: "queue--mail", TaskID: uniqueID("busy-task"), ActionKind: "send", Status: persistence.BrokerActionStaged, ArgsJSON: []byte(`{}`), ExpiresAt: s.now.Add(time.Hour)}
	if _, err = s.actions.Stage(s.ctx, a); !errors.Is(err, persistence.ErrAccountTransition) {
		t.Fatalf("busy stage %v", err)
	}
}

func (s queuedActionSuite) replacement(t *testing.T) {
	a := s.stage(t)
	before, err := s.actions.Get(s.ctx, a.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.AccountGuard) != 2 {
		t.Fatal("stored vector absent")
	}
	s.rotate(t, "ns:queue")
	if ok, err := s.actions.Stage(s.ctx, a); err != nil || !ok {
		t.Fatalf("restage %v %v", ok, err)
	}
	fresh, err := s.actions.Get(s.ctx, a.ActionID)
	if err != nil || reflect.DeepEqual(fresh.AccountGuard, before.AccountGuard) {
		t.Fatal("replacement did not capture new generation")
	}
	s.promote(t, a)
	s.rotate(t, "ns:queue")
	if ok, err := s.actions.Stage(s.ctx, a); err != nil || ok {
		t.Fatalf("pending replacement %v %v", ok, err)
	}
	got, err := s.actions.Get(s.ctx, a.ActionID)
	if err != nil || !reflect.DeepEqual(got.AccountGuard, fresh.AccountGuard) {
		t.Fatal("pending generation mutated")
	}
}

func (s queuedActionSuite) collision(t *testing.T) {
	a := s.stage(t)
	candidate := *a
	candidate.ProjectID = "other--mail"
	if ok, err := s.actions.Stage(s.ctx, &candidate); err != nil || ok {
		t.Fatalf("cross-project replacement %v %v", ok, err)
	}
	got, err := s.actions.Get(s.ctx, a.ActionID)
	if err != nil || got.ProjectID != a.ProjectID {
		t.Fatal("collision changed project")
	}
}

func (s queuedActionSuite) operator(t *testing.T) {
	a := s.stage(t)
	s.promote(t, a)
	if err := s.actions.Approve(s.ctx, a.ActionID, a.ArgsSHA256, "device", s.now); err != nil {
		t.Fatal(err)
	}
	s.rotate(t, "ns:queue")
	got, err := s.actions.Get(s.ctx, a.ActionID)
	if err != nil || got.Status != persistence.BrokerActionApproved {
		t.Fatal("terminal success lost")
	}
	op := *a
	op.ActionID = ""
	op.ProjectID = "operator"
	op.TaskID = uniqueID("op-task")
	if _, err := s.actions.Stage(s.ctx, &op); err != nil {
		t.Fatal(err)
	}
	s.promote(t, &op)
	if err := s.actions.Approve(s.ctx, op.ActionID, op.ArgsSHA256, "operator", s.now); err != nil {
		t.Fatal(err)
	}
	got, err = s.actions.Get(s.ctx, op.ActionID)
	if err != nil || got.AccountGuard != nil {
		t.Fatal("operator acquired agent vector")
	}
}

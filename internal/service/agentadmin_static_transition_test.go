package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

type staticGrantList struct {
	persistence.BrokerGrantRepository
	cap  int
	fail error
	seen persistence.BrokerGrantFilter
}

func (r *staticGrantList) List(ctx context.Context, f persistence.BrokerGrantFilter) ([]*persistence.BrokerStandingGrant, error) {
	r.seen = f
	if r.fail != nil {
		return nil, r.fail
	}
	if r.cap > 0 {
		f.LiveCap = r.cap
	}
	return r.BrokerGrantRepository.List(ctx, f)
}

func staticTransitionFixture(t *testing.T) (*agentAdminFixture, []*persistence.BrokerStandingGrant) {
	t.Helper()
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "static"}, map[string]*agentadmin.AccountInput{"mail": {Label: "work"}, "other": {Label: "other"}})
	return f, []*persistence.BrokerStandingGrant{seedGrant(t, f, "mail"), seedGrant(t, f, "other")}
}

func TestAgentAccount_StaticSuspendsHiddenNamespace(t *testing.T) {
	f, gs := staticTransitionFixture(t)
	ctx := context.Background()
	// Foreign grants are real persisted rows, copied from the valid seed.
	for _, item := range []struct{ id, ns string }{{"static-operator", ""}, {"static-foreign", "other-agent"}} {
		_, err := f.c.DB.ExecContext(ctx, `INSERT INTO broker_standing_grants SELECT ?, project_id, ?, workflow_id, action, key_paths, key_values, key_hash, max_uses, uses_left, expires_at, created_at, created_by, seed_action_id, reach_hash_at_creation, active, paused, suspended_at, revoked_at, digest_through, account_label, account_identity FROM broker_standing_grants WHERE id=?`, item.id, item.ns, gs[0].ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.c.Registry = nil
	if err := f.svc.storeCredential(ctx, approvalPayload{Slot: &agentadmin.CredentialSlot{Namespace: "hermes", Name: "MAIL_TOKEN"}}, f.device.ID, []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		if !suspended(t, f, g.ID) {
			t.Fatal("hidden namespace grant retained authority")
		}
	}
	for _, id := range []string{"static-operator", "static-foreign"} {
		if suspended(t, f, id) {
			t.Fatal("foreign scope suspended")
		}
	}
}

func TestAgentAccount_StaticEnumerationFailuresKeepBusy(t *testing.T) {
	for _, mode := range []string{"truncated", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := staticTransitionFixture(t)
			ctx := context.Background()
			st, err := f.c.secretStoreForWrite()
			if err != nil {
				t.Fatal(err)
			}
			before, err := st.Get(ctx, "hermes", "MAIL_TOKEN")
			if err != nil {
				t.Fatal(err)
			}
			r := &staticGrantList{BrokerGrantRepository: f.c.repos.BrokerGrants, cap: 1}
			if mode == "read-error" {
				r.cap = 0
				r.fail = errors.New("list unavailable")
			}
			f.c.repos.BrokerGrants = r
			err = f.svc.storeCredential(ctx, approvalPayload{Slot: &agentadmin.CredentialSlot{Namespace: "hermes", Name: "MAIL_TOKEN"}}, f.device.ID, []byte("blocked"))
			if err == nil {
				t.Fatal("incomplete enumeration published credential")
			}
			after, getErr := st.Get(ctx, "hermes", "MAIL_TOKEN")
			if getErr != nil || string(before) != string(after) {
				t.Fatal("failed suspension changed secret")
			}
			if _, err = f.c.accountDecisionEpoch(ctx, "hermes"); !errors.Is(err, persistence.ErrAccountTransition) {
				t.Fatalf("transition did not stay busy: %v", err)
			}
		})
	}
}

func TestAgentAccount_StaticEnumerationCountsPresuspended(t *testing.T) {
	f, gs := staticTransitionFixture(t)
	ctx := context.Background()
	if _, err := f.c.repos.BrokerGrants.Suspend(ctx, gs[0].ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	r := &staticGrantList{BrokerGrantRepository: f.c.repos.BrokerGrants}
	f.c.repos.BrokerGrants = r
	n, err := f.c.suspendNamespaceGrants(ctx, "hermes")
	if err != nil || n != 2 {
		t.Fatalf("examined=%d, err=%v", n, err)
	}
	if r.seen.Namespace != "hermes" || r.seen.Now.IsZero() || r.seen.Limit != 1 {
		t.Fatal("complete namespace list contract changed")
	}
	if !suspended(t, f, gs[1].ID) {
		t.Fatal("second live grant hidden by dead-tail limit")
	}
}

func TestAgentAccount_StaticNewEntryAlsoSuspends(t *testing.T) {
	f, gs := staticTransitionFixture(t)
	ctx := context.Background()
	f.c.Registry = nil
	if err := f.svc.storeCredential(ctx, approvalPayload{Slot: &agentadmin.CredentialSlot{Namespace: "hermes", Name: "NEW_KEY"}}, f.device.ID, []byte("new-value")); err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		if !suspended(t, f, g.ID) {
			t.Fatal("new static key bypassed full namespace suspension")
		}
	}
	st, err := f.c.secretStoreForWrite()
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, "hermes", "NEW_KEY")
	if err != nil || string(got) != "new-value" {
		t.Fatal("successful new entry was not published")
	}
}

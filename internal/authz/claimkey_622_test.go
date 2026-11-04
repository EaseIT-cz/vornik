package authz

// GitHub #622 (decision 2026-11-02, option 2): a key mapped to a person is
// that person's console login. A plain key that carries a workflow list is an
// A2A caller key (#613 C1 confines it to its A2A workflows), so it must never
// become a login, and the mapping is refused at both writers. Companion keys
// keep mapping with a list: they are mapped for attribution and cannot reach
// the session exchange. Found at: review (automated security review of the
// pushed #613 code); a user would observe it at the production HTTP surface.
// Design: oidc-identity-permissions-design.md, Amendment 2026-11-02 (#622).

import (
	"context"
	"errors"
	"testing"
)

func (f *claimFixture) keyWith(t *testing.T, id, secret, clientKind string, workflows []string) string {
	t.Helper()
	f.key(t, id, secret)
	f.keys.rows[id].ClientKind = clientKind
	f.keys.rows[id].AllowedWorkflows = workflows
	return id
}

func TestClaimKey622_RefusesAPlainKeyWithAWorkflowList(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	narrowed := f.keyWith(t, "akey_a2a", "sk-vornik-a2a", "", []string{"vornik-architecture"})
	wrong := f.key(t, "akey_plain", "sk-vornik-plain")

	f.keys.calls, f.repo.calls = nil, nil
	err := f.accounts.ClaimKey(ctx, f.owner.ID, narrowed, "sk-vornik-a2a", Actor{Principal: "owner"})
	if !errors.Is(err, ErrKeyClaimRefused) {
		t.Fatalf("claim of a plain key with a workflow list: err = %v, want ErrKeyClaimRefused", err)
	}
	if _, bound := f.repo.bindings["api_key:"+narrowed]; bound {
		t.Fatalf("a plain key with a workflow list was mapped to an account")
	}
	narrowedCalls := len(f.keys.calls) + len(f.repo.calls)

	// The refusal must be the same one a wrong secret gets, with the same
	// lookups (§5.4's oracle rule): a claimant learns nothing about the key.
	f.keys.calls, f.repo.calls = nil, nil
	wrongErr := f.accounts.ClaimKey(ctx, f.owner.ID, wrong, "sk-vornik-WRONG", Actor{Principal: "owner"})
	if wrongErr == nil || err.Error() != wrongErr.Error() {
		t.Errorf("refusal text %q differs from the wrong-secret refusal %v", err, wrongErr)
	}
	if got := len(f.keys.calls) + len(f.repo.calls); got != narrowedCalls {
		t.Errorf("the narrowed-key refusal did %d lookups, a wrong secret %d: both must do the same work", narrowedCalls, got)
	}
}

func TestAssignKey622_RefusesAPlainKeyWithAWorkflowList(t *testing.T) {
	f := newClaimFixture(t)
	narrowed := f.keyWith(t, "akey_a2a", "sk-vornik-a2a", "", []string{"vornik-architecture"})
	err := f.accounts.AssignKey(context.Background(), f.owner.ID, narrowed, Actor{Principal: "admin", Source: "cli"})
	if !errors.Is(err, ErrKeyNotALogin) {
		t.Fatalf("admin assign of a plain key with a workflow list: err = %v, want ErrKeyNotALogin", err)
	}
	if _, bound := f.repo.bindings["api_key:"+narrowed]; bound {
		t.Fatalf("a plain key with a workflow list was mapped by admin assign")
	}
}

func TestClaimKey622_OtherKeysStillMap(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	companion := f.keyWith(t, "akey_cc", "sk-vornik-cc", "claude-code", []string{"companion-doc-review"})
	plain := f.key(t, "akey_plain", "sk-vornik-plain")
	if err := f.accounts.ClaimKey(ctx, f.owner.ID, companion, "sk-vornik-cc", Actor{Principal: "owner"}); err != nil {
		t.Fatalf("a companion key with a list is mapped for attribution: %v", err)
	}
	if err := f.accounts.AssignKey(ctx, f.owner.ID, plain, Actor{Principal: "admin"}); err != nil {
		t.Fatalf("a plain key without a list is a login and maps: %v", err)
	}
	for _, id := range []string{companion, plain} {
		if f.repo.bindings["api_key:"+id] != f.owner.ID {
			t.Errorf("%s was not mapped", id)
		}
	}
}

// TestClaimKey622_AgentKeyWithAListIsRefused pins the boundary between this
// predicate and #613 C1 (review-20261102-056b): an agent key carrying a list
// would also be refused; a per-task agent key is never a person's login.
func TestClaimKey622_AgentKeyWithAListIsRefused(t *testing.T) {
	f := newClaimFixture(t)
	agent := f.keyWith(t, "akey_agent", "sk-vornik-agent", "", []string{"wf"})
	f.keys.rows[agent].Name = "agent:task_t1"
	err := f.accounts.AssignKey(context.Background(), f.owner.ID, agent, Actor{Principal: "admin"})
	if !errors.Is(err, ErrKeyNotALogin) {
		t.Fatalf("admin assign of an agent key with a list: err = %v, want ErrKeyNotALogin", err)
	}
}

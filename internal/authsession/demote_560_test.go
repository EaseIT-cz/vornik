package authsession

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/listener"
)

// GitHub #560 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1a)", A4b; M1b B1): in managed hosting an admin's session is admin
// only on the operations listener; anywhere else it is tenant_admin, the
// instance owner, which every reader of the session role places explicitly.

func roleOn(t *testing.T, b *SessionBackend, l listener.Name) string {
	t.Helper()
	id, err := b.Authenticate(listener.With(context.Background(), l), auth.Credential{SessionToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	role, _ := id.Extra[auth.ExtraSessionRole].(string)
	return role
}

func notOperations(ctx context.Context) bool { return listener.FromContext(ctx) != listener.Operations }

func TestSessionDemotion560(t *testing.T) {
	admin := &authz.Principal{Role: authz.RoleAdmin}
	b := NewSessionBackend(&stubValidator{sess: liveSession()}, &stubResolver{p: admin}, 0)
	b.DemoteAdmin = notOperations

	for _, order := range [][]listener.Name{
		{listener.Public, listener.Operations, listener.Public},
		{listener.Operations, listener.Public, listener.Operations},
	} {
		for _, l := range order {
			want := auth.RoleTenantAdmin
			if l == listener.Operations {
				want = auth.RoleAdmin
			}
			if got := roleOn(t, b, l); got != want {
				t.Fatalf("admin session on %q: role %q, want %q", l, got, want)
			}
		}
	}
	for _, l := range []listener.Name{listener.AgentSocket, listener.Unknown} {
		if got := roleOn(t, b, l); got != auth.RoleTenantAdmin {
			t.Fatalf("admin session on %q: role %q, want tenant_admin (only the operations listener admits admin)", l, got)
		}
	}
	if admin.Role != authz.RoleAdmin {
		t.Fatal("the demotion mutated the principal")
	}

	// A user's session is a user's on every listener.
	ub := NewSessionBackend(&stubValidator{sess: liveSession()}, &stubResolver{p: &authz.Principal{Role: authz.RoleUser}}, 0)
	ub.DemoteAdmin = notOperations
	if got := roleOn(t, ub, listener.Operations); got != auth.RoleUser {
		t.Fatalf("a user's session on the operations listener became %q", got)
	}

	// Self-hosted: no predicate, admin everywhere.
	sb := NewSessionBackend(&stubValidator{sess: liveSession()}, &stubResolver{p: admin}, 0)
	for _, l := range []listener.Name{listener.Public, listener.Unknown} {
		if got := roleOn(t, sb, l); got != auth.RoleAdmin {
			t.Fatalf("self-hosted admin session on %q: role %q", l, got)
		}
	}
}

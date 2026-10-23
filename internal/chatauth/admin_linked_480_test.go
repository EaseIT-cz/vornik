package chatauth

// GitHub #480 (operator decision A5 and its second-opinion review): an
// admin-linked chat keeps the raw error text, and "admin-linked" is the
// authoritative check, a resolved principal with the admin role, never the
// legacy allowlist (a wildcard allowlist entry is not an admin). Found at:
// review; the channel tests (telegram, service) pin it where a speaker
// reaches it.

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/authz"
)

func TestDecision480_AdminLinked(t *testing.T) {
	admin := &authz.Principal{UserID: "u-admin", Role: authz.RoleAdmin, Projects: []string{"*"}}
	user := &authz.Principal{UserID: "u-user", Role: authz.RoleUser, Projects: []string{"*"}}
	r := stubResolver{principals: map[string]*authz.Principal{
		"telegram:1": admin,
		"telegram:2": user,
	}}
	shim, _, _ := newShim(t, r)
	wildcard := Legacy{Allowed: true, ConfigKey: "telegram.allowed_users", Projects: nil}
	ctx := context.Background()

	cases := []struct {
		name string
		d    Decision
		want bool
	}{
		{"linked admin", shim.Authorize(ctx, "telegram", "1", Legacy{}), true},
		{"linked admin, also on the legacy list", shim.Authorize(ctx, "telegram", "1", wildcard), true},
		{"linked user with every project", shim.Authorize(ctx, "telegram", "2", wildcard), false},
		{"wildcard legacy entry, not linked", shim.Authorize(ctx, "telegram", "3", wildcard), false},
		{"unknown and not listed", shim.Authorize(ctx, "telegram", "4", Legacy{}), false},
	}
	down, _, _ := newShim(t, stubResolver{err: errors.New("db down")})
	cases = append(cases,
		struct {
			name string
			d    Decision
			want bool
		}{"resolver outage, legacy grant", down.Authorize(ctx, "telegram", "1", wildcard), false})
	// No shim wired: the channel builds the decision from the legacy list.
	cases = append(cases, struct {
		name string
		d    Decision
		want bool
	}{"no resolver", Decision{Allowed: true}, false})
	// A principal on a refused decision is never admin (defence in depth).
	cases = append(cases, struct {
		name string
		d    Decision
		want bool
	}{"refused with a principal", Decision{Allowed: false, Principal: admin}, false})

	for _, c := range cases {
		if got := c.d.AdminLinked(); got != c.want {
			t.Errorf("%s: AdminLinked() = %v, want %v", c.name, got, c.want)
		}
	}
}

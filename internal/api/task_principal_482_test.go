package api

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #482 and #499 (design 22, part 8a). A REST task created by a static
// key recorded no principal and counted as the operator's; it now records
// system:static_key:<id> (never trusted) unless the key is the operator's
// listed admin key (system:operator_key:<id>). Round-3 review A1: when a
// request carries a key AND a session, the key decides, so a companion key
// presented beside an admin session cannot be stamped as the session user.
func TestTaskPrincipal(t *testing.T) {
	bg := context.Background()
	const opKey, brokerKey = "sk-vornik-operator-secret", "sk-vornik-broker-secret"
	admin := config.AdminConfig{Enabled: true, AllowedKeys: []string{opKey, brokerKey}, OperatorKeyIDs: []string{config.KeyHashID(opKey)}}
	s := &Server{adminClassKeys: admin}
	session := func(ctx context.Context) context.Context {
		return context.WithValue(ctx, identityKey, &auth.Identity{Subject: "u", Extra: map[string]any{auth.ExtraSessionUserID: "user-7"}})
	}

	broker := s.taskCreatorActor(context.WithValue(bg, apiKeyKey, brokerKey))
	if !strings.HasPrefix(broker.String(), persistence.StaticKeyPrincipalPrefix) || strings.Contains(broker.String(), "secret") {
		t.Fatalf("broker key principal = %q", broker.String())
	}
	op := s.taskCreatorActor(context.WithValue(bg, apiKeyKey, opKey))
	if op.String() != persistence.OperatorKeyPrincipalPrefix+config.KeyHashID(opKey) {
		t.Fatalf("operator key principal = %q", op.String())
	}
	// A listed id on a key that is not admin-class is not the operator's.
	notAdmin := &Server{adminClassKeys: config.AdminConfig{Enabled: true, OperatorKeyIDs: []string{config.KeyHashID(opKey)}}}
	if a := notAdmin.taskCreatorActor(context.WithValue(bg, apiKeyKey, opKey)); strings.HasPrefix(a.String(), persistence.OperatorKeyPrincipalPrefix) {
		t.Fatalf("a non-admin key was stamped as the operator's: %q", a.String())
	}
	if a := s.taskCreatorActor(context.WithValue(bg, apiKeyIDKey, "akey-1")); !a.IsZero() {
		t.Fatalf("a DB key is attributed by the creator from its id; got %q", a.String())
	}
	if a := s.taskCreatorActor(session(bg)); a.String() != "user:user-7" {
		t.Fatalf("session principal = %q", a.String())
	}
	// Both a key and a session: the key decides (A1).
	if a := s.taskCreatorActor(session(context.WithValue(bg, apiKeyIDKey, "akey-companion"))); !a.IsZero() {
		t.Fatalf("a DB key beside a session was stamped %q", a.String())
	}
	if a := s.taskCreatorActor(session(context.WithValue(bg, apiKeyKey, brokerKey))); !strings.HasPrefix(a.String(), persistence.StaticKeyPrincipalPrefix) {
		t.Fatalf("a static key beside a session was stamped %q", a.String())
	}
	if a := s.taskCreatorActor(bg); !a.IsZero() {
		t.Fatalf("nothing known: %q", a.String())
	}
}

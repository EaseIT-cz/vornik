package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #571 U3b, mcp-server-authentication-design.md §15.3a C5. Found at:
// design review (the code read that the injection path consults only the
// token row); a user observes it as a re-added connection acting on the
// removed account's mailbox. Regression test at: integration (the service
// over a real database) — the row the injection path reads is gone. The
// delete commits in the removal's own transaction (Finish), which the
// drivers' TestAccountTransition571_* tests pin.

func seedSignIn571(t *testing.T, f *agentAdminFixture, server string) {
	t.Helper()
	exp := time.Now().Add(time.Hour).UTC()
	if err := f.c.repos.MCPOAuthTokens.Upsert(context.Background(), &persistence.MCPOAuthToken{
		ProjectID: "hermes--comms", ServerName: server, Resource: "https://r", ClientID: "c",
		AccessToken: "sv1:old-account", RefreshToken: "sv1:old-refresh", ExpiresAt: &exp, ConnectedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func signInStored571(t *testing.T, f *agentAdminFixture, server string) bool {
	t.Helper()
	_, err := f.c.repos.MCPOAuthTokens.Get(context.Background(), "hermes--comms", server, "")
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func TestRemoveConnection571_DeletesItsSignIn(t *testing.T) {
	f := accountGrantFixture(t, agentadmin.MCPAuthInput{Mode: "none"}, map[string]*agentadmin.AccountInput{
		"mail-work": {Label: "work"}, "mail-personal": {Label: "personal"}})
	seedSignIn571(t, f, "mail-work")
	seedSignIn571(t, f, "mail-work-write") // the write sibling's own row
	seedSignIn571(t, f, "mail-personal")
	if res := f.do(agentadmin.VerbRemove, agentadmin.RemoveInput{Kind: "integration", Project: "comms", ID: "mail-work"}); res.Effect != agentadmin.EffectApplied {
		t.Fatalf("remove: %+v", res)
	}
	if signInStored571(t, f, "mail-work") {
		t.Fatal("the removed connection's sign-in is still stored: a same-name re-add would be served it")
	}
	if signInStored571(t, f, "mail-work-write") {
		t.Fatal("the removed connection's write sibling kept its sign-in")
	}
	if !signInStored571(t, f, "mail-personal") {
		t.Fatal("another connection's sign-in was deleted")
	}
}

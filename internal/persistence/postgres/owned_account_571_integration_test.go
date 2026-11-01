//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #571 U3b (mcp-server-authentication-design.md §15.3a C6; found at:
// design review, multi-tenancy §6 round 5): the visible-label check and the
// write run under the connection's advisory lock, so a create held between
// its check and its write keeps a cross-class create of the same label out.
func TestOwnedAccount571_LabelCheckIsSerialised(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("race%d--mail", time.Now().UnixNano()%1_000_000)
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	first := NewAgentGrantRepository(db.DB)
	first.beforeLabelWrite = func() { once.Do(func() { close(held); <-release }) }
	second := NewAgentGrantRepository(db.DB)
	base := persistence.AgentIntegrationApproval{Namespace: project[:len(project)-6], ProjectID: project, Integration: "gmail",
		Kind: "mcp", URL: "u", ApprovedByDevice: "d", ApprovedAt: time.Now().UTC(), AccountLabel: "work"}

	firstErr := make(chan error, 1)
	go func() {
		owned := base
		owned.OwnerUserID = "alice"
		firstErr <- first.CreateOwnedIntegration(ctx, owned)
	}()
	<-held
	secondErr := make(chan error, 1)
	go func() { secondErr <- second.UpsertIntegration(ctx, base) }()
	select {
	case err := <-secondErr:
		t.Fatalf("the shared create ran while the owned create held the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("the owned create: %v", err)
	}
	if err := <-secondErr; !errors.Is(err, persistence.ErrAccountLabelTaken) {
		t.Fatalf("the shared create of the same label: %v, want ErrAccountLabelTaken", err)
	}
}

// GitHub #571 U3b (§15.3a C3): migration 237 gave every existing row a
// unique, non-empty id and kept it a shared account.
func TestOwnedAccount571_MigrationGaveEveryRowAnID(t *testing.T) {
	db := newIntegrationDB(t)
	for _, table := range []string{"mcp_oauth_tokens", "agent_integration_approvals"} {
		var rows, distinct, empty int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*), count(DISTINCT id), count(*) FILTER (WHERE id = '') FROM `+table).
			Scan(&rows, &distinct, &empty); err != nil {
			t.Fatal(err)
		}
		// Shared-ness is not asserted here: the owner suites on this
		// database create owned rows of their own projects. The SQLite
		// rebuild test pins that every pre-existing row stays shared.
		if rows != distinct || empty != 0 {
			t.Fatalf("%s: %d rows, %d distinct ids, %d empty", table, rows, distinct, empty)
		}
	}
}

// The removal's sign-in delete is in Finish's transaction (§15.3a C5).
func TestAccountTransition571_SignInDeletedWithTheRemoval(t *testing.T) {
	db := accountTransitionFixture(t)
	ctx := context.Background()
	tokens := NewMCPOAuthTokenRepository(db.DB)
	if err := tokens.Upsert(ctx, &persistence.MCPOAuthToken{ProjectID: "hermes--mail", ServerName: "gmail", Resource: "r", ClientID: "c", AccessToken: "sv1:x"}); err != nil {
		t.Fatal(err)
	}
	grants := NewAgentGrantRepository(db.DB)
	if err := grants.UpsertIntegration(ctx, persistence.AgentIntegrationApproval{Namespace: "hermes", ProjectID: "hermes--mail", Integration: "gmail",
		Kind: "mcp", URL: "u", ApprovedByDevice: "d", ApprovedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := NewAccountTransitionRepository(db.DB)
	ticket, err := r.Begin(ctx, persistence.AccountNamespaceScope("hermes"), "owner", "config", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := persistence.AccountPublication{
		DeleteTokens: []persistence.AccountTokenKey{{ProjectID: "hermes--mail", ServerName: "gmail"}},
		Removed:      []persistence.AccountIntegrationRemoval{{ProjectID: "hermes--mail", Integration: "gmail", At: time.Now()}},
		Identity:     &persistence.AccountIdentityUpdate{ProjectID: "missing", Integration: "missing", Identity: "x"},
	}
	if err = r.Finish(ctx, ticket, p); err == nil {
		t.Fatal("a publication with a missing identity target succeeded")
	}
	if _, err = tokens.Get(ctx, "hermes--mail", "gmail", ""); err != nil {
		t.Fatalf("a failed publication deleted the sign-in: %v", err)
	}
	p.Identity = nil
	if err = r.Finish(ctx, ticket, p); err != nil {
		t.Fatal(err)
	}
	if _, err = tokens.Get(ctx, "hermes--mail", "gmail", ""); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the published removal kept the sign-in: %v", err)
	}
	if a, err := grants.GetIntegration(ctx, "hermes--mail", "gmail"); err != nil || a.RemovedAt == nil {
		t.Fatalf("the approval after the published removal: %+v %v", a, err)
	}
}

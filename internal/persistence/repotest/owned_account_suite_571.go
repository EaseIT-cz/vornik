package repotest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #571 U3b (mcp-server-authentication-design.md §15.3a C1, C4, C6):
// one connection holds one shared account and at most one per owner. These
// suites run on both drivers.

// RunMCPOAuthOwnerSuite pins the token table's owner key: a shared and an
// owned grant of one (project, server) are distinct rows, every keyed method
// addresses exactly the row its owner names, and an upsert of the same owner
// replaces its own row only.
func RunMCPOAuthOwnerSuite(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	t.Helper()
	t.Run("Shared_and_owned_rows_are_distinct", func(t *testing.T) { ownerDistinctRows(t, repo) })
	t.Run("Keyed_methods_address_one_owner", func(t *testing.T) { ownerKeyedMethods(t, repo) })
	t.Run("Swap_guards_one_owner", func(t *testing.T) { ownerSwap(t, repo) })
}

func ownedToken(project, server, owner, access string) *persistence.MCPOAuthToken {
	tok := mcpToken(project, server)
	tok.OwnerUserID, tok.AccessToken, tok.RefreshToken = owner, access, "rt-"+access
	return tok
}

func ownerDistinctRows(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	project := uniqueID("own")
	for _, tok := range []*persistence.MCPOAuthToken{
		ownedToken(project, "mail", "", "shared-1"),
		ownedToken(project, "mail", "alice", "alice-1"),
		ownedToken(project, "mail", "bob", "bob-1"),
		ownedToken(project, "mail", "alice", "alice-2"), // replaces alice's row only
		ownedToken(project, "mail", "", "shared-2"),     // replaces the shared row only
	} {
		if err := repo.Upsert(ctx, tok); err != nil {
			t.Fatalf("Upsert %q: %v", tok.OwnerUserID, err)
		}
	}
	for owner, want := range map[string]string{"": "shared-2", "alice": "alice-2", "bob": "bob-1"} {
		got, err := repo.Get(ctx, project, "mail", owner)
		if err != nil || got.AccessToken != want || got.OwnerUserID != owner {
			t.Fatalf("Get owner %q = %+v, %v; want access %q", owner, got, err, want)
		}
	}
	if _, err := repo.Get(ctx, project, "mail", "carol"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("an owner with no row: %v, want ErrNotFound", err)
	}
	list, err := repo.ListForProject(ctx, project)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListForProject = %d rows, %v; want every owner's (3)", len(list), err)
	}
	all, err := repo.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tok := range all {
		if tok.ProjectID == project {
			seen++
		}
	}
	if seen != 3 {
		t.Fatalf("ListAll carries %d of the project's rows, want 3", seen)
	}
}

func ownerKeyedMethods(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	project := uniqueID("ownkey")
	for _, owner := range []string{"", "alice"} {
		if err := repo.Upsert(ctx, ownedToken(project, "cal", owner, "at-"+owner)); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.MarkNeedsReconnect(ctx, project, "cal", "alice"); err != nil {
		t.Fatal(err)
	}
	shared, _ := repo.Get(ctx, project, "cal", "")
	alice, _ := repo.Get(ctx, project, "cal", "alice")
	if shared.NeedsReconnect || !alice.NeedsReconnect {
		t.Fatalf("MarkNeedsReconnect(alice): shared %v, alice %v", shared.NeedsReconnect, alice.NeedsReconnect)
	}
	won, err := repo.ReplaceTokens(ctx, project, "cal", "", shared.AccessToken, shared.RefreshToken, "AT-S", "RT-S", false)
	if err != nil || !won {
		t.Fatalf("ReplaceTokens(shared): %v %v", won, err)
	}
	if a, _ := repo.Get(ctx, project, "cal", "alice"); a.AccessToken != alice.AccessToken {
		t.Fatalf("ReplaceTokens(shared) changed alice's row: %q", a.AccessToken)
	}
	if err := repo.Delete(ctx, project, "cal", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, project, "cal", ""); err != nil {
		t.Fatalf("a second Delete of an absent row: %v, want nil (a retried removal completes)", err)
	}
	if _, err := repo.Get(ctx, project, "cal", ""); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("after Delete(shared): %v", err)
	}
	if _, err := repo.Get(ctx, project, "cal", "alice"); err != nil {
		t.Fatalf("Delete(shared) removed alice's row: %v", err)
	}
	locked := false
	if err := repo.WithRefreshLock(ctx, project, "cal", "alice", func(context.Context) error { locked = true; return nil }); err != nil || !locked {
		t.Fatalf("WithRefreshLock(owner): %v %v", locked, err)
	}
}

func ownerSwap(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	project := uniqueID("ownswap")
	for _, owner := range []string{"", "alice"} {
		if err := repo.Upsert(ctx, ownedToken(project, "drive", owner, "same")); err != nil {
			t.Fatal(err)
		}
	}
	next := ownedToken(project, "drive", "alice", "rotated")
	won, err := repo.SwapRefreshToken(ctx, "rt-same", next)
	if err != nil || !won {
		t.Fatalf("Swap(alice): %v %v", won, err)
	}
	if s, _ := repo.Get(ctx, project, "drive", ""); s.AccessToken != "same" {
		t.Fatalf("Swap(alice) changed the shared row, which held the same refresh token: %q", s.AccessToken)
	}
}

// RunAgentGrantOwnerSuite pins the approvals table's owner key and the
// visible-label rule (§15.3a C1, C6) on both drivers.
func RunAgentGrantOwnerSuite(t *testing.T, repo persistence.AgentGrantRepository) {
	t.Helper()
	ctx := context.Background()
	ns := "o" + uniqueTail(strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(uniqueID("o"))))
	project := ns + "--mail"
	now := time.Now().UTC().Truncate(time.Second)
	base := persistence.AgentIntegrationApproval{Namespace: ns, ProjectID: project, Integration: "gmail", Kind: "mcp",
		URL: "https://mail.example/mcp", ReadTools: []string{"list"}, ApprovedByDevice: "dev_a", ApprovedAt: now, AccountLabel: "team"}
	if err := repo.UpsertIntegration(ctx, base); err != nil {
		t.Fatal(err)
	}
	AssertMiss(t, "AgentGrantRepository.GetOwnedIntegration", func() (*persistence.AgentIntegrationApproval, error) {
		return repo.GetOwnedIntegration(ctx, project, "gmail", "nobody")
	})

	alice := base
	alice.OwnerUserID, alice.AccountLabel = "alice", "team"
	if err := repo.CreateOwnedIntegration(ctx, alice); !errors.Is(err, persistence.ErrAccountLabelTaken) {
		t.Fatalf("an owned account with the live shared account's label: %v, want ErrAccountLabelTaken", err)
	}
	alice.AccountLabel = "work"
	if err := repo.CreateOwnedIntegration(ctx, alice); err != nil {
		t.Fatalf("CreateOwnedIntegration: %v", err)
	}
	if err := repo.CreateOwnedIntegration(ctx, alice); !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("a second owned account of one owner: %v, want ErrDuplicateKey", err)
	}
	unowned := base
	unowned.OwnerUserID = ""
	if err := repo.CreateOwnedIntegration(ctx, unowned); err == nil {
		t.Fatal("CreateOwnedIntegration without an owner succeeded")
	}

	got, err := repo.GetIntegration(ctx, project, "gmail")
	if err != nil || got.OwnerUserID != "" || got.AccountLabel != "team" {
		t.Fatalf("GetIntegration addresses the shared row: %+v, %v", got, err)
	}
	owned, err := repo.GetOwnedIntegration(ctx, project, "gmail", "alice")
	if err != nil || owned.OwnerUserID != "alice" || owned.AccountLabel != "work" {
		t.Fatalf("GetOwnedIntegration: %+v, %v", owned, err)
	}
	list, err := repo.ListIntegrations(ctx, ns)
	if err != nil || len(list) != 1 || list[0].OwnerUserID != "" {
		t.Fatalf("ListIntegrations lists the shared rows only: %+v, %v", list, err)
	}

	// Removing the shared account leaves the owned one; a re-add under the
	// owned account's label is refused, under another reuses the row.
	if err := repo.MarkIntegrationRemoved(ctx, project, "gmail", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if o, _ := repo.GetOwnedIntegration(ctx, project, "gmail", "alice"); o == nil || o.RemovedAt != nil {
		t.Fatalf("removing the shared account removed alice's: %+v", o)
	}
	readd := base
	readd.ApprovedAt, readd.AccountLabel = now.Add(2*time.Minute), "work"
	if err := repo.UpsertIntegration(ctx, readd); !errors.Is(err, persistence.ErrAccountLabelTaken) {
		t.Fatalf("a shared re-add with an owned account's label: %v, want ErrAccountLabelTaken", err)
	}
	readd.AccountLabel = "office"
	if err := repo.UpsertIntegration(ctx, readd); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetIntegration(ctx, project, "gmail"); got == nil || got.RemovedAt != nil || got.AccountLabel != "office" {
		t.Fatalf("the re-add reuses the shared row with the new label: %+v", got)
	}
	if list, _ := repo.ListIntegrations(ctx, ns); len(list) != 1 {
		t.Fatalf("a re-add left %d shared rows, want 1", len(list))
	}
}

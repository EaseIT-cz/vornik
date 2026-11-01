package repotest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// RunSealedMCPOAuthTokenSuite exercises the agent token sealing wrapper
// (agent-administered Vornik plan P4.4) over a driver's token repository:
// wrapped is the wrapper, inner the raw repository it wraps. Run from BOTH
// drivers: the rotation guard is the inner conditional UPDATE, called with
// the stored sealed value, and its row count is dialect-specific.
func RunSealedMCPOAuthTokenSuite(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository) {
	t.Helper()
	project := fmt.Sprintf("rtseal--p%d", atomic.AddUint64(&uniqueCounter, 1))
	t.Run("Get_miss_obeys_the_contract", func(t *testing.T) {
		AssertMiss(t, "MCPOAuthTokenRepository.Get", func() (*persistence.MCPOAuthToken, error) {
			return wrapped.Get(context.Background(), project, uniqueID("absent"), "")
		})
	})
	t.Run("tokens_are_sealed_in_the_row_and_open_through_the_wrapper", func(t *testing.T) { runSealedRoundTrip(t, wrapped, inner, project) })
	t.Run("swap_wins_once_and_keeps_the_consent", func(t *testing.T) { runSealedSwap(t, wrapped, project) })
	t.Run("concurrent_swaps_win_once", func(t *testing.T) { runSealedConcurrentSwap(t, wrapped, project) })
	// GitHub #571 U3a (mcp-server-authentication-design.md §15.2): operator
	// projects and the daemon scope are sealed too, in the operator domain.
	t.Run("operator_and_daemon_rows_are_sealed", func(t *testing.T) { runSealedOperatorRows(t, wrapped, inner) })
	t.Run("operator_swap_wins_once", func(t *testing.T) { runSealedSwap(t, wrapped, uniqueID("opswap")) })
	t.Run("operator_value_is_bound_to_its_row", func(t *testing.T) { runSealedOperatorBinding(t, wrapped, inner) })
	t.Run("an_unsealed_row_is_refused", func(t *testing.T) { runSealedRefusesPlaintext(t, wrapped, inner) })
	// GitHub #571 U3b (§15.3a C2): an owned row is sealed under its own
	// label, and no value moves between a shared and an owned row.
	t.Run("owned_rows_round_trip", func(t *testing.T) { runSealedOwnedRoundTrip(t, wrapped, inner, project) })
	t.Run("owned_and_shared_values_do_not_cross", func(t *testing.T) { runSealedOwnedBinding(t, wrapped, inner, project) })
}

func runSealedOwnedRoundTrip(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository, agentProject string) {
	ctx := context.Background()
	for _, project := range []string{agentProject, uniqueID("opowned"), ""} {
		server := uniqueID("cal")
		tok := mcpToken(project, server)
		tok.OwnerUserID, tok.AccessToken, tok.RefreshToken = "alice", "AT-OWN-CANARY", "RT-OWN-CANARY"
		if err := wrapped.Upsert(ctx, tok); err != nil {
			t.Fatalf("Upsert owned %q: %v", project, err)
		}
		raw, err := inner.Get(ctx, project, server, "alice")
		if err != nil || !strings.HasPrefix(raw.AccessToken, "sv1:") || strings.Contains(raw.AccessToken, "CANARY") {
			t.Fatalf("project %q: the owned row holds %+v, %v", project, raw, err)
		}
		got, err := wrapped.Get(ctx, project, server, "alice")
		if err != nil || got.AccessToken != "AT-OWN-CANARY" || got.RefreshToken != "RT-OWN-CANARY" {
			t.Fatalf("project %q: Get owned %+v %v", project, got, err)
		}
	}
}

// Each pair is (server, owner) sealed at the first and moved to the second;
// it must not open there. The agent shared label is unescaped, so these are
// the cases an added owner segment alone would have collided on.
func runSealedOwnedBinding(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository, agentProject string) {
	ctx := context.Background()
	type acct struct{ server, owner string }
	pairs := [][2]acct{
		{{"x/o", ""}, {"x", "o"}},
		{{"x", "o"}, {"x/o", ""}},
		{{"x/U_y", ""}, {"x", "U_y"}},
		{{"u/u", ""}, {"u", "u"}},
		{{"u", "u"}, {"u/u", ""}},
		{{"same", ""}, {"same", "alice"}},
		{{"same", "alice"}, {"same", ""}},
		{{"same", "alice"}, {"same", "bob"}},
	}
	for _, project := range []string{agentProject, uniqueID("opbind"), ""} {
		for i, pr := range pairs {
			from, to := pr[0], pr[1]
			prefix := fmt.Sprintf("b%d-%d", atomic.AddUint64(&uniqueCounter, 1), i)
			src := mcpToken(project, prefix+from.server)
			src.OwnerUserID = from.owner
			if err := wrapped.Upsert(ctx, src); err != nil {
				t.Fatal(err)
			}
			raw, err := inner.Get(ctx, project, prefix+from.server, from.owner)
			if err != nil {
				t.Fatal(err)
			}
			moved := *raw
			moved.ServerName, moved.OwnerUserID = prefix+to.server, to.owner
			if err := inner.Upsert(ctx, &moved); err != nil {
				t.Fatal(err)
			}
			if got, err := wrapped.Get(ctx, project, prefix+to.server, to.owner); err == nil {
				t.Fatalf("project %q: a value sealed for %+v opened for %+v: %+v", project, from, to, got)
			}
		}
	}
}

func runSealedRoundTrip(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository, project string) {
	ctx := context.Background()
	tok := mcpToken(project, "bank")
	tok.AccessToken, tok.RefreshToken = "AT-CANARY-1", "RT-CANARY-1"
	if err := wrapped.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	raw, err := inner.Get(ctx, project, "bank", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{raw.AccessToken, raw.RefreshToken} {
		if !strings.HasPrefix(v, "sv1:") || strings.Contains(v, "CANARY") {
			t.Fatalf("the row holds %q, not a sealed value", v)
		}
	}
	if raw.Resource != tok.Resource || raw.Scopes != tok.Scopes || raw.ConnectedBy != tok.ConnectedBy || raw.ClientID != tok.ClientID {
		t.Fatalf("a plaintext column changed: %+v", raw)
	}
	got, err := wrapped.Get(ctx, project, "bank", "")
	if err != nil || got.AccessToken != "AT-CANARY-1" || got.RefreshToken != "RT-CANARY-1" {
		t.Fatalf("Get: %+v %v", got, err)
	}
	list, err := wrapped.ListForProject(ctx, project)
	if err != nil || len(list) == 0 || list[0].AccessToken != "AT-CANARY-1" {
		t.Fatalf("ListForProject: %+v %v", list, err)
	}
}

func runSealedSwap(t *testing.T, wrapped persistence.MCPOAuthTokenRepository, project string) {
	ctx := context.Background()
	tok := mcpToken(project, "mail")
	tok.RefreshToken = "rt-1"
	if err := wrapped.Upsert(ctx, tok); err != nil {
		t.Fatal(err)
	}
	next := mcpToken(project, "mail")
	next.AccessToken, next.RefreshToken, next.ConnectedBy = "at-winner", "rt-2", "someone-else"
	if won, err := wrapped.SwapRefreshToken(ctx, "rt-1", next); err != nil || !won {
		t.Fatalf("the swap with the stored refresh token lost: %v", err)
	}
	stale := mcpToken(project, "mail")
	stale.AccessToken, stale.RefreshToken = "at-loser", "rt-3"
	if won, err := wrapped.SwapRefreshToken(ctx, "rt-1", stale); err != nil || won {
		t.Fatalf("a rotated-away refresh token won: %v", err)
	}
	got, _ := wrapped.Get(ctx, project, "mail", "")
	if got.AccessToken != "at-winner" || got.RefreshToken != "rt-2" || got.ConnectedBy != tok.ConnectedBy {
		t.Fatalf("after the swaps: %+v", got)
	}
}

func runSealedConcurrentSwap(t *testing.T, wrapped persistence.MCPOAuthTokenRepository, project string) {
	ctx := context.Background()
	tok := mcpToken(project, "chat")
	tok.RefreshToken = "rt-2"
	if err := wrapped.Upsert(ctx, tok); err != nil {
		t.Fatal(err)
	}
	// Concurrent refreshers that all used rt-2: exactly one wins.
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := mcpToken(project, "chat")
			n.AccessToken, n.RefreshToken = fmt.Sprintf("at-c%d", i), fmt.Sprintf("rt-c%d", i)
			won, err := wrapped.SwapRefreshToken(ctx, "rt-2", n)
			if err != nil {
				t.Error(err)
			}
			if won {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d concurrent swaps won, want exactly 1", wins.Load())
	}
}

func runSealedOperatorRows(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	for _, project := range []string{uniqueID("operator"), ""} {
		server := uniqueID("jira")
		tok := mcpToken(project, server)
		tok.AccessToken, tok.RefreshToken = "AT-OP-CANARY", "RT-OP-CANARY"
		if err := wrapped.Upsert(ctx, tok); err != nil {
			t.Fatalf("Upsert %q: %v", project, err)
		}
		raw, err := inner.Get(ctx, project, server, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{raw.AccessToken, raw.RefreshToken} {
			if !strings.HasPrefix(v, "sv1:") || strings.Contains(v, "CANARY") {
				t.Fatalf("project %q: the row holds %q, not a sealed value", project, v)
			}
		}
		got, err := wrapped.Get(ctx, project, server, "")
		if err != nil || got.AccessToken != "AT-OP-CANARY" || got.RefreshToken != "RT-OP-CANARY" {
			t.Fatalf("project %q: Get %+v %v", project, got, err)
		}
	}
}

// A sealed operator value opens only for the (project, server) it was sealed
// for, and the label encoding is injective: (a/b, c) is not (a, b/c).
func runSealedOperatorBinding(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	base := uniqueID("bind")
	pairs := [][2]string{{base + "/x", "y"}, {base, "x/y"}}
	if err := wrapped.Upsert(ctx, mcpToken(pairs[0][0], pairs[0][1])); err != nil {
		t.Fatal(err)
	}
	raw, err := inner.Get(ctx, pairs[0][0], pairs[0][1], "")
	if err != nil {
		t.Fatal(err)
	}
	moved := *raw
	moved.ProjectID, moved.ServerName = pairs[1][0], pairs[1][1]
	if err := inner.Upsert(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	if got, err := wrapped.Get(ctx, pairs[1][0], pairs[1][1], ""); err == nil {
		t.Fatalf("a value sealed for %v opened for %v: %+v", pairs[0], pairs[1], got)
	}
}

// After U3a no read hands out a plaintext token found in the table.
func runSealedRefusesPlaintext(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	project, server := uniqueID("plain"), "jira"
	if err := inner.Upsert(ctx, mcpToken(project, server)); err != nil {
		t.Fatal(err)
	}
	if got, err := wrapped.Get(ctx, project, server, ""); err == nil {
		t.Fatalf("an unsealed row was served: %+v", got)
	}
	if _, err := wrapped.ListForProject(ctx, project); err == nil {
		t.Fatal("ListForProject served an unsealed row")
	}
}

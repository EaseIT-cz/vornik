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
			return wrapped.Get(context.Background(), project, uniqueID("absent"))
		})
	})
	t.Run("tokens_are_sealed_in_the_row_and_open_through_the_wrapper", func(t *testing.T) { runSealedRoundTrip(t, wrapped, inner, project) })
	t.Run("swap_wins_once_and_keeps_the_consent", func(t *testing.T) { runSealedSwap(t, wrapped, project) })
	t.Run("concurrent_swaps_win_once", func(t *testing.T) { runSealedConcurrentSwap(t, wrapped, project) })
	t.Run("operator_projects_pass_through", func(t *testing.T) { runSealedOperatorPassThrough(t, wrapped, inner) })
}

func runSealedRoundTrip(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository, project string) {
	ctx := context.Background()
	tok := mcpToken(project, "bank")
	tok.AccessToken, tok.RefreshToken = "AT-CANARY-1", "RT-CANARY-1"
	if err := wrapped.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	raw, err := inner.Get(ctx, project, "bank")
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
	got, err := wrapped.Get(ctx, project, "bank")
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
	got, _ := wrapped.Get(ctx, project, "mail")
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

func runSealedOperatorPassThrough(t *testing.T, wrapped, inner persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	op := uniqueID("operator")
	tok := mcpToken(op, "jira")
	if err := wrapped.Upsert(ctx, tok); err != nil {
		t.Fatal(err)
	}
	raw, err := inner.Get(ctx, op, "jira")
	if err != nil || raw.AccessToken != tok.AccessToken {
		t.Fatalf("an operator token was changed: %+v %v", raw, err)
	}
}

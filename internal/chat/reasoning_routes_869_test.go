package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// GitHub #869, reasoning control design B2/B4, slice 2: the Claude and Codex
// subscription routes and the Claude CLI map a request's level, and a request
// level overrides the route's own setting (thinking_budget, effort_level);
// with no level the route setting is sent as before.
//
// Found at: production (companion reviews failed on the output cap; a role
// could not set reasoning on any route). Regression test at: unit. Lower
// because: each mapping is decided in its converter.

func claudeThinking(ctx context.Context, t *testing.T, opts ...ClaudeSubscriptionOption) (map[string]any, int) {
	t.Helper()
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6", opts...)
	raw, err := c.buildRequestBodyCtx(ctx, []Message{{Role: "user", Content: "hi"}}, nil, claudeAccountInfo{}, "s")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	th, _ := body["thinking"].(map[string]any)
	mt, _ := body["max_tokens"].(float64)
	return th, int(mt)
}

func TestClaudeSubscription_LevelMapsAndOverridesTheRoute_869(t *testing.T) {
	bg := context.Background()
	// No level: the route's budget, as before.
	if th, _ := claudeThinking(bg, t, WithClaudeSubscriptionMaxTokens(16384), WithClaudeSubscriptionThinkingBudget(8192)); th == nil || th["budget_tokens"] != float64(8192) {
		t.Fatalf("no level must keep the route budget 8192, got %v", th)
	}
	// off overrides a route budget: no thinking block.
	if th, _ := claudeThinking(WithReasoningEffort(bg, ReasoningEffortOff), t, WithClaudeSubscriptionMaxTokens(16384), WithClaudeSubscriptionThinkingBudget(8192)); th != nil {
		t.Fatalf("off must send no thinking block, got %v", th)
	}
	// low/medium/high, clamped to max_tokens/2 (8192 -> at most 4096).
	for level, want := range map[string]float64{ReasoningEffortLow: 1024, ReasoningEffortMedium: 4096, ReasoningEffortHigh: 4096} {
		th, mt := claudeThinking(WithReasoningEffort(bg, level), t, WithClaudeSubscriptionMaxTokens(8192))
		if th == nil || th["budget_tokens"] != want {
			t.Errorf("%s at max_tokens 8192: thinking = %v, want budget %v", level, th, want)
		}
		if b, _ := th["budget_tokens"].(float64); int(b) >= mt {
			t.Errorf("%s: budget %v must stay below max_tokens %d", level, b, mt)
		}
	}
	// Under the 1024 floor: no block (answered approximated).
	if th, _ := claudeThinking(WithReasoningEffort(bg, ReasoningEffortHigh), t, WithClaudeSubscriptionMaxTokens(1500)); th != nil {
		t.Fatalf("max_tokens 1500 cannot hold a 1024 budget below half; want no block, got %v", th)
	}
}

func TestCodexSubscription_LevelOverridesTheRoute_869(t *testing.T) {
	for _, tc := range []struct{ route, level, want string }{
		{"low", "", "low"},                   // no level: the route's
		{"low", ReasoningEffortHigh, "high"}, // a level overrides it
		{"high", ReasoningEffortOff, "low"},  // off: Codex's lowest, approximated
		{"", "", ""},                         // nothing either way: no reasoning block
	} {
		got := codexEffort(WithReasoningEffort(context.Background(), tc.level), tc.route)
		if got != tc.want {
			t.Errorf("route %q level %q: effort = %q, want %q", tc.route, tc.level, got, tc.want)
		}
	}
}

func TestClaudeCLI_LevelOverridesTheRoute_869(t *testing.T) {
	for _, tc := range []struct{ route, level, want string }{
		{"low", "", "low"},
		{"low", ReasoningEffortHigh, "high"},
		{"medium", ReasoningEffortOff, "low"},
	} {
		if got := cliEffort(WithReasoningEffort(context.Background(), tc.level), tc.route); got != tc.want {
			t.Errorf("route %q level %q: effort = %q, want %q", tc.route, tc.level, got, tc.want)
		}
	}
}

// The table now says what these routes do.
func TestReasoningTable_SliceTwoRoutes_869(t *testing.T) {
	for _, tc := range []struct {
		kind, level string
		want        ReasoningSupport
	}{
		{"claude-subscription", ReasoningEffortOff, ReasoningSent},
		{"claude-subscription", ReasoningEffortHigh, ReasoningSent},
		{"codex-subscription", ReasoningEffortOff, ReasoningApproximated},
		{"codex-subscription", ReasoningEffortMedium, ReasoningSent},
		{"claude-cli", ReasoningEffortOff, ReasoningApproximated},
		{"claude-cli", ReasoningEffortLow, ReasoningSent},
		{"codex-cli", ReasoningEffortLow, ReasoningUnsupported},
	} {
		if got, _ := ReasoningSupportFor(tc.kind, tc.level); got != tc.want {
			t.Errorf("%s/%s = %q, want %q", tc.kind, tc.level, got, tc.want)
		}
	}
	_ = strings.TrimSpace
}

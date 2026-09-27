package agenttools

import "testing"

// TestRoleAllowsTool pins the CLOSED-WORLD semantics of the server-side role
// MCP-tool gate (api roleAllowsMCPTool, via RoleAllowsTool). Moved here from
// internal/api on 2026-09-25 with the matcher itself, unchanged, so the
// executor's input-staging decision shares it (media routing LLD §4.2a).
//
// A role with a non-empty allowlist may invoke ONLY what it lists. MCP tools
// must be granted explicitly — by name, bare segment, or an mcp__* /
// mcp__server__* wildcard the operator writes deliberately. There is no
// fail-open by omission (a deliberately-narrow built-in-only role must NOT
// reach project MCP tools like broker place_order).
//
// Regression context (2026-06-20): the janka `researcher` listed only built-in
// tools, so it could not call mcp__scraper__web_fetch and every portal scan got
// a daemon-level FORBIDDEN → stale RAG. The fix is to GRANT the tool in the
// role config (deployed + distributed presets), which the cases below pin.
func TestRoleAllowsTool(t *testing.T) {
	builtinOnly := []string{"file_read", "file_write", "run_shell", "grep", "memory_search", "current_time"}
	// The FIX applied to research roles: built-ins + the explicit scraper grant.
	researcherFixed := append(append([]string{}, builtinOnly...), "mcp__scraper__web_fetch", "mcp__scraper__ical_events")
	// Trading role: explicit, least-privilege broker tools (real ibkr pattern).
	tradingRO := []string{"current_time", "memory_search", "mcp__broker__get_quote", "mcp__broker__get_positions"}
	serverWildcard := []string{"current_time", "mcp__scraper__*"}
	allMCPWildcard := []string{"file_read", "mcp__*"}

	cases := []struct {
		name      string
		allowed   []string
		tool      string
		wantAllow bool
	}{
		// Closed-world: built-in-only role does NOT get MCP tools (no fail-open).
		{"builtin-only role DENIED scraper web_fetch", builtinOnly, "mcp__scraper__web_fetch", false},
		// The config fix grants it explicitly.
		{"researcher with grant allows web_fetch", researcherFixed, "mcp__scraper__web_fetch", true},
		{"researcher with grant allows ical_events", researcherFixed, "mcp__scraper__ical_events", true},
		// B2 preserved: explicit MCP allowlist denies unlisted tools.
		{"trading role denies unlisted place_order", tradingRO, "mcp__broker__place_order", false},
		{"trading role allows listed get_quote", tradingRO, "mcp__broker__get_quote", true},
		// Server wildcard grants that server, nothing else.
		{"server wildcard allows scraper tool", serverWildcard, "mcp__scraper__web_fetch", true},
		{"server wildcard denies other server", serverWildcard, "mcp__broker__place_order", false},
		// Global mcp wildcard defers all MCP to the project layer.
		{"mcp__* wildcard allows any mcp tool", allMCPWildcard, "mcp__broker__place_order", true},
		// Built-in gating unchanged.
		{"builtin not in list is denied", builtinOnly, "run_shell_unlisted", false},
		{"builtin in list is allowed", builtinOnly, "grep", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoleAllowsTool(tc.allowed, tc.tool); got != tc.wantAllow {
				t.Fatalf("RoleAllowsTool(%v, %q) = %v, want %v", tc.allowed, tc.tool, got, tc.wantAllow)
			}
		})
	}
}

// TestAllowlistAdmits — the empty rule is a separate named step. An empty
// allowlist means "no narrowing" everywhere in the system (/mcp/call, the
// advertised catalogue, tool grants); the bare matcher on an empty slice admits
// nothing. The empty rule used to live before the matcher in internal/api
// (roleToolAllowlistReason's role_declares_none) and in two more call sites as
// `len(x) == 0 ||`; it lives here so the executor cannot reach a different
// answer than the gate (media routing LLD §4.2a, review-20260925-0430 round 2).
func TestAllowlistAdmits(t *testing.T) {
	const outline = "mcp__vornik__document_get_outline"
	if RoleAllowsTool([]string{}, outline) || RoleAllowsTool(nil, outline) {
		t.Fatal("the bare matcher must admit nothing on an empty allowlist")
	}
	if !AllowlistAdmits(nil, outline) || !AllowlistAdmits([]string{}, outline) {
		t.Fatal("an empty allowlist must admit everything")
	}
	if AllowlistAdmits([]string{"file_read"}, outline) {
		t.Fatal("a non-empty allowlist narrows exactly as the matcher does")
	}
	if !AllowlistAdmits([]string{"mcp__vornik__*"}, outline) {
		t.Fatal("a server wildcard admits that server's tools")
	}
}

// TestBareToolName — the one normalisation of the two namespace spellings.
func TestBareToolName(t *testing.T) {
	for in, want := range map[string]string{
		"mcp__vornik__document_read_section": "document_read_section",
		"functions.git_status":               "git_status",
		"git_status":                         "git_status",
		"mcp__scraper__web.fetch":            "fetch",
	} {
		if got := BareToolName(in); got != want {
			t.Errorf("BareToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

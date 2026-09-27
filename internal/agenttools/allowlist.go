package agenttools

import "strings"

// The role tool-allowlist predicate. Moved here from internal/api on
// 2026-09-25 (media routing LLD §4.2a) so the executor's input-staging
// decision and the /mcp/call gate share ONE predicate: a second copy of a
// security matcher is the anti-pattern behind the 2026.8.1 bypass.

// AllowlistAdmits reports whether a role allowlist admits qualifiedName. An
// EMPTY allowlist admits everything: a role that declares no allowedTools is not
// narrowed, which is the rule /mcp/call, the advertised catalogue and tool
// grants all apply. Otherwise the answer is RoleAllowsTool's. The empty rule is
// a named step here, not a property of the matcher: RoleAllowsTool on an empty
// slice admits nothing.
func AllowlistAdmits(allowed []string, qualifiedName string) bool {
	if len(allowed) == 0 {
		return true
	}
	return RoleAllowsTool(allowed, qualifiedName)
}

// BareToolName reduces a tool name to its bare segment across both namespace
// spellings: MCP's "__" (mcp__vornik__file_read) and the OpenAI-compatible
// function schema's "." (functions.file_read).
func BareToolName(name string) string {
	if idx := strings.LastIndex(name, "__"); idx >= 0 {
		name = name[idx+2:]
	}
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	return name
}

// RoleAllowsTool applies a resolved (non-empty) role allowlist to one
// requested tool. Pure (no I/O) so the policy is unit-testable without
// standing up task → execution → workflow → role resolution.
//
// CLOSED-WORLD (B2 authorization gate): a role with a non-empty allowlist may
// invoke only the tools it lists. MCP-qualified tools (mcp__server__tool) must
// be granted EXPLICITLY — by exact name, by bare segment, or by a wildcard the
// operator writes deliberately:
//
//	mcp__*           → any MCP tool (defer MCP gating to the project layer:
//	                   permissions.allowedTools + the MCP server's allowed_tools)
//	mcp__server__*   → any tool of one MCP server
//
// There is NO fail-open by omission: listing only built-in tools does NOT
// grant MCP tools (that would let a deliberately-narrow role reach, e.g.,
// broker place_order whenever the project enables it). A role that should use
// project MCP tools declares that intent with mcp__* or the specific tools.
//
// Trading roles keep the strict intersection (listing mcp__broker__get_quote
// still denies mcp__broker__place_order). Regression context: the janka
// `researcher` listed only built-in tools and so could not call
// mcp__scraper__web_fetch — every portal scan got a daemon-level FORBIDDEN,
// starving the RAG (2026-06-20). The fix is to GRANT the tool in the role
// config (here + the distributed swarm presets), not to fail open.
func RoleAllowsTool(allowed []string, qualifiedName string) bool {
	// Bare tool segment so an allowlist authored as either the qualified or the
	// bare name both match.
	//
	// TWO namespace conventions reach this check, and only one used to be
	// handled. MCP qualifies with "__" (mcp__vornik__file_read); the
	// OpenAI-compatible function schema qualifies with "." (functions.file_read),
	// and that is the form a model emits when it names a tool back to us. Handling
	// only "__" meant every grant request phrased the second way was refused
	// against a ceiling of bare names — so a reviewer asking for
	// "functions.git_status" was denied a tool its role plainly allows, retried
	// with four different spellings, and burned nine tool calls failing. Found by
	// the agent-quality benchmark against real refusal rows, 2026-08-14.
	//
	// This widens matching only in the direction the operator already intended:
	// an allowlist entry "file_read" means the file_read tool however the caller
	// spelled it. Exact entries are still matched first and are unaffected.
	bare := BareToolName(qualifiedName)
	isMCP := strings.HasPrefix(qualifiedName, "mcp__")
	for _, a := range allowed {
		if a == qualifiedName || a == bare {
			return true
		}
		if isMCP && mcpWildcardMatch(a, qualifiedName) {
			return true
		}
	}
	return false
}

// mcpWildcardMatch reports whether an allowlist entry is an MCP wildcard that
// covers qualifiedName (already known to start with "mcp__"). Supported:
// "mcp__*" (all MCP tools) and "mcp__<server>__*" (one server's tools). The
// operator must write the wildcard explicitly — absence never grants.
func mcpWildcardMatch(entry, qualifiedName string) bool {
	if entry == "mcp__*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(entry, "*"); ok {
		// e.g. entry "mcp__broker__*" → prefix "mcp__broker__"
		if strings.HasPrefix(prefix, "mcp__") && strings.HasPrefix(qualifiedName, prefix) {
			return true
		}
	}
	return false
}

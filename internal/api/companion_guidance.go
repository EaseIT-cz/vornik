package api

import (
	_ "embed"
	"strings"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// companionGuidance is how an agent should work alongside Vornik (companion
// RAG-first guidance design §10): delegate first, never wait idle, verify,
// recall, park side tasks, write ideas down. It is the one source: the
// daemon serves it as initialize instructions, and the claude-code and codex
// delegate skills carry it between the markers below (a test compares them).
//
//go:embed companion_guidance.md
var companionGuidance string

// The markers that delimit the guidance inside a delegate SKILL.md; the
// SessionStart hook prints what lies between them.
const (
	companionGuidanceStart = "<!-- vornik-guidance:start -->"
	companionGuidanceEnd   = "<!-- vornik-guidance:end -->"
)

// adminDelegationScope keeps an admin agent from delegating its own setup
// work (review e485 F3).
const adminDelegationScope = "Your setup work (projects, roles, workflows, connections, credentials, budgets) is your own part and is never delegated; delegate the user's work to the workflows you build."

// CompanionGuidance returns the guidance text.
func CompanionGuidance() string { return strings.TrimSpace(companionGuidance) }

// initializeInstructions is what initialize tells a key (design §10 item 2):
// an agent admin key the admin guidance, the scoping sentence and the
// companion text; any other key not on a broker project the companion text
// (a memory-only key included: the text's self-gate drops the delegation
// rules); a broker-project key nothing, because a broker serves one request
// and its vornik-broker skill already says how.
func (s *Server) initializeInstructions(key *persistence.APIKey) string {
	if s.agentAdminOffered(key) {
		return agentadmin.AdminGuidance() + "\n\n" + adminDelegationScope + "\n\n" + CompanionGuidance()
	}
	if s.isBrokerProjectKey(key) {
		return ""
	}
	return CompanionGuidance()
}

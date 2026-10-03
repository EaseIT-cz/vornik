package agentadmin

import (
	_ "embed"
	"strings"
)

// adminGuidance is the one source of how an agent should work with the admin
// verbs (plan P6.3; review 20261002-6f6b F3). It is served as the companion
// endpoint's initialize instructions and as describe_installation's
// how_to_work, and the claude-code, codex and hermes bundles' vornik-admin
// skills carry the same text (a test compares them), because no harness is known to
// surface every one of those places to the model.
//
//go:embed admin_guidance.md
var adminGuidance string

// AdminGuidance returns the guidance text.
func AdminGuidance() string { return strings.TrimSpace(adminGuidance) }

// RecipeRule is design §19.2's one guidance rule: a shipped, tested recipe
// before a hand-written workflow.
const RecipeRule = "Prefer a recipe when one fits; author a workflow only when none does."

// HandoffRule is how work moves between a workflow's steps (design §18.10).
// The guidance and define_workflow's steps description both state it, and
// tests hold each to this exact sentence.
const HandoffRule = "Steps hand work to each other only through files under `artifacts/out/`; only the last step writes `result.json`."

// ReconnectRule is design §18.14 finding 2's guidance line (round 2
// wording): a client that took the tool schemas before a change refuses a
// field the daemon now accepts, and only reconnecting refreshes it.
const ReconnectRule = "If your client refuses a field `describe_installation` lists, your client holds the tool list from before a change: ask the user to reconnect Vornik in the client."

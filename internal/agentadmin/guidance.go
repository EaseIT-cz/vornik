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

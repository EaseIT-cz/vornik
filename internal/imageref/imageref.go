// Package imageref holds the one mapping from the legacy agent image name to
// the canonical one (2026-10-02-easeit-org-migration-design.md §5.2). It is a
// leaf package with no dependencies, so both internal/runtime (which must not
// import imagemanifest) and imagemanifest use the same implementation.
package imageref

import (
	"strings"
	"sync"
)

// The legacy agent image name (2026-10-02-easeit-org-migration-design.md
// §5.2). The image moved from ghcr.io/grinco to ghcr.io/easeit-cz, and every
// historical tag was copied with its digest, so a reference by the old name
// resolves to the same bytes under the new one. Deployed configs are never
// overwritten (config-deploy is preserve-existing), so they keep the old name
// indefinitely; this alias is permanent for that reason (design §5.2).
const (
	// LegacyAgentRepo is the agent image repository before the move.
	LegacyAgentRepo = "ghcr.io/grinco/vornik-agent"
	// AgentRepo is the canonical agent image repository.
	AgentRepo = "ghcr.io/easeit-cz/vornik-agent"
	// AgentRegistry is AgentRepo's registry path, the prefix a bare
	// `vornik-agent` short-name qualifies to.
	AgentRegistry = "ghcr.io/easeit-cz/"
)

// Canonical maps a reference to the legacy agent repository onto the
// canonical one, keeping its tag or digest. Every other reference, including
// other images of the old owner and names that merely share the prefix, is
// returned unchanged. Idempotent.
func Canonical(ref string) string {
	rest, ok := strings.CutPrefix(ref, LegacyAgentRepo)
	if !ok {
		return ref
	}
	// Exactly the repository: end of string, a tag, or a digest. Anything
	// else (vornik-agent-x, vornik-agent/sub) is a different repository.
	if rest == "" || rest[0] == ':' || rest[0] == '@' {
		return AgentRepo + rest
	}
	return ref
}

// CanonicalFrom is Canonical for a reference read from a
// named source (a config file, a setting), recording the rewrite once per
// reference per process so it is visible rather than silent.
func CanonicalFrom(ref, source string) string {
	canonical := Canonical(ref)
	if canonical != ref {
		noteLegacyImage(ref, canonical, source)
	}
	return canonical
}

var (
	legacyImageMu     sync.Mutex
	legacyImageLogger func(old, canonical, source string)
	legacyImageSeen   = map[string]bool{}
)

// SetLegacyImageLogger installs the sink for legacy-name rewrites (the daemon
// wires its logger) and returns a function that restores the previous one.
func SetLegacyImageLogger(fn func(old, canonical, source string)) (restore func()) {
	legacyImageMu.Lock()
	defer legacyImageMu.Unlock()
	prev := legacyImageLogger
	legacyImageLogger = fn
	return func() {
		legacyImageMu.Lock()
		defer legacyImageMu.Unlock()
		legacyImageLogger = prev
	}
}

func noteLegacyImage(old, canonical, source string) {
	legacyImageMu.Lock()
	if legacyImageSeen[old] || legacyImageLogger == nil {
		legacyImageMu.Unlock()
		return
	}
	legacyImageSeen[old] = true
	fn := legacyImageLogger
	legacyImageMu.Unlock()
	fn(old, canonical, source)
}

// ResetLegacyImageNotes forgets which references were logged (tests).
func ResetLegacyImageNotes() {
	legacyImageMu.Lock()
	defer legacyImageMu.Unlock()
	legacyImageSeen = map[string]bool{}
}

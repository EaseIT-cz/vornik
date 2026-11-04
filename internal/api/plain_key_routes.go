package api

import (
	"net/http"
	"path"
	"slices"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// plainKeyRefusal is the third database-key confinement in AuthMiddleware,
// beside the companion and agent ones (GitHub #613). A plain key (no client
// kind, not an agent key: what `vornikctl key create` mints) was a full key
// on every route of its project, whatever its capability flags said. It
// returns the refusal code and message, or "" when the request may proceed:
//
//   - C1: a key with a workflow list is an A2A key. It may call only
//     /a2a/v1/agents/{its project}/{a listed workflow}[/...], on a clean path;
//     a2aScopeGuard still checks project and workflow behind it.
//   - C2: memory routes read the flags. GET or HEAD on
//     /api/v1/projects/{p}/memory/... needs memory_read; any other method
//     there, and /api/v1/projects/{p}/artifacts/{id}/extract (it chunks into
//     memory), needs memory_write.
//   - C3: a database key does not manage keys: /api/v1/projects/{p}/keys[/...],
//     and the self-service companion mint, /api/v1/companion/grant and /keys
//     (Round 2).
//   - C2b (Round 2): the project-scoped daemon-wide memory routes
//     (memoryDaemonRoutes) follow C2, and /api/v1/projects/{p}/gist (top terms
//     over the project's memory) is a read.
//
// C2 and C3 classify the cleaned path, so a double slash, a dot segment or a
// trailing slash cannot step around them. Static keys, admin-class keys and UI
// sessions are not database keys and never reach this function.
//
// see LLD § https://docs.vornik.io, Amendment 2026-11-01 (#613)
func plainKeyRefusal(row *persistence.APIKey, method, p string) (code, message string) {
	if row == nil || row.ClientKind != "" || isAgentKeyRow(row) {
		return "", ""
	}
	if len(row.AllowedWorkflows) > 0 {
		if isListedA2ARoute(row, p) {
			return "", ""
		}
		return "KEY_WORKFLOW_SCOPED", "a key limited to workflows may only call them over A2A"
	}
	clean := path.Clean("/" + p)
	if clean == "/api/v1/companion/grant" || clean == "/api/v1/companion/keys" {
		return keyManagementRefusal()
	}
	if memoryDaemonRoutes[clean] {
		return memoryRefusal(row, method)
	}
	sub, last, ok := projectSubroute(clean)
	if !ok {
		return "", ""
	}
	switch {
	case sub == "keys":
		return keyManagementRefusal()
	case sub == "memory":
		return memoryRefusal(row, method)
	case sub == "gist":
		return memoryRefusal(row, http.MethodGet)
	case sub == "artifacts" && last == "extract":
		return memoryRefusal(row, http.MethodPost)
	}
	return "", ""
}

// memoryDaemonRoutes are the daemon-wide /api/v1/memory/ routes a project key
// can reach (their handlers gate on requestAllowsProject). The other
// registrations under /api/v1/memory/ are operator surface with their own
// gate, which answers a project-scoped key 404 so the route stays hidden
// (#495); refusing them here with a 403 would reveal them.
var memoryDaemonRoutes = map[string]bool{
	"/api/v1/memory/stats":          true,
	"/api/v1/memory/reclassify-llm": true,
	"/api/v1/memory/regraph":        true,
	// With ?project= the quarantine re-flag accepts a project key (round 3).
	"/api/v1/memory/regraph/quarantined": true,
}

func keyManagementRefusal() (code, message string) {
	return "KEY_MANAGEMENT_OPERATOR_ONLY",
		"a database key cannot create, list or change keys; use a static key or the UI"
}

// memoryRefusal applies the C2 rule: GET or HEAD reads memory, any other
// method writes it.
func memoryRefusal(row *persistence.APIKey, method string) (code, message string) {
	if method == http.MethodGet || method == http.MethodHead {
		if !row.MemoryRead {
			return "MEMORY_CAPABILITY_REQUIRED", "this key lacks memory_read"
		}
		return "", ""
	}
	if !row.MemoryWrite {
		return "MEMORY_CAPABILITY_REQUIRED", "this key lacks memory_write"
	}
	return "", ""
}

// isListedA2ARoute reports whether p is an A2A agent route of the key's own
// project and one of its listed workflows. An unclean path never matches.
func isListedA2ARoute(row *persistence.APIKey, p string) bool {
	const prefix = "/a2a/v1/agents/"
	if p == "" || path.Clean(p) != p || !strings.HasPrefix(p, prefix) {
		return false
	}
	segs := strings.Split(strings.TrimPrefix(p, prefix), "/")
	if len(segs) < 2 || segs[0] != row.ProjectID || segs[1] == "" {
		return false
	}
	return slices.Contains(row.AllowedWorkflows, segs[1])
}

// projectSubroute splits an already cleaned /api/v1/projects/{p}/{sub}/.../{last}
// path. ok is false outside the project tree or with no segment after {p}.
func projectSubroute(clean string) (sub, last string, ok bool) {
	const prefix = "/api/v1/projects/"
	if !strings.HasPrefix(clean, prefix) {
		return "", "", false
	}
	segs := strings.Split(strings.TrimPrefix(clean, prefix), "/")
	if len(segs) < 2 || segs[0] == "" || segs[1] == "" {
		return "", "", false
	}
	return segs[1], segs[len(segs)-1], true
}

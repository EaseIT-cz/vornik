package api

import (
	"net/http"
	"strings"

	"vornik.io/vornik/internal/auth"
)

// The instance owner reads the operator console (18-security-and-tenancy.md,
// amendment 2026-11-15 (#787)): on a managed instance, a tenant_admin
// session may GET or HEAD the console's observation pages, never change
// anything through it, and never read the pages that configure the daemon or
// identities. OwnerConsoleRead is the whole decision; the admin gate and the
// router's re-check both call it.

// OwnerConsoleReadablePaths are the console pages the owner may read, as the
// router's switch writes them (the path with /ui and /admin stripped).
var OwnerConsoleReadablePaths = map[string]bool{
	"/":                       true,
	"/audit":                  true,
	"/chat-audit":             true,
	"/memory-audit":           true,
	"/health":                 true,
	"/health/leases":          true,
	"/health/watchdog":        true,
	"/health/cluster":         true,
	"/cpc":                    true,
	"/instincts":              true,
	"/skills":                 true,
	"/control-plane":          true,
	"/control-plane/diagnose": true,
	"/blackbox":               true,
	"/blackbox/overrides":     true,
	"/blackbox/candidates":    true,
	"/memory/firewall":        true,
	"/workflow-proposals":     true,
	"/agents":                 true,
}

// OwnerConsoleRefusedPaths are the console pages the owner may not even read:
// the VM's runtime, daemon configuration, the daemon MCP catalog (with its
// hosts), admin keys and accounts. Listed so the census test can require a
// decision for every router path.
var OwnerConsoleRefusedPaths = map[string]bool{
	"/health/runtime":                 true,
	"/health/mcp":                     true,
	"/integrations/mcp":               true,
	"/integrations/email":             true,
	"/integrations/dispatcher-tools":  true,
	"/control-plane/mcp":              true,
	"/control-plane/mcp/probe":        true,
	"/control-plane/mcp/use":          true,
	"/blackbox/overrides/save":        true,
	"/blackbox/overrides/delete":      true,
	"/blackbox/triggers/bulk-dismiss": true,
	"/keys":                           true,
	"/users":                          true,
}

// OwnerConsoleDetailPatterns are the detail pages the owner may read: the
// prefix and how many id segments follow it. An action is a further segment
// (`/workflow-proposals/<id>/decide`), so it never matches.
var OwnerConsoleDetailPatterns = map[string]int{
	"/blackbox/":               1,
	"/blackbox/triggers/":      1,
	"/blackbox/candidates/":    1,
	"/blackbox/compare/":       2,
	"/memory/firewall/chunks/": 1,
	"/workflow-proposals/":     1,
	"/agents/":                 1,
}

// ownerConsoleActionNames are segments that name an action route, never an
// id: refused even where they would fill an id slot.
var ownerConsoleActionNames = map[string]bool{
	"bulk-dismiss": true, "dismiss": true, "generate-candidate": true,
	"run-trial": true, "promote": true, "reject": true, "save": true, "delete": true,
	"decide": true, "apply": true, "rollback": true, "modify": true, "withdraw": true, "cancel": true, "retire": true,
}

// AdminRouterPath is the path the console router dispatches on: the request
// path inside the /ui subtree with the /admin prefix removed, "/" for the
// landing page. One helper, so the gate and the router judge the same string.
func AdminRouterPath(uiPath string) string {
	p := strings.TrimPrefix(uiPath, "/admin")
	if p == "" {
		return "/"
	}
	return p
}

// OwnerConsoleRead reports whether r, a request for routerPath, is an owner's
// read the console admits: a tenant_admin session, the operator_console_read
// surface granted, GET or HEAD, and a readable page.
func OwnerConsoleRead(r *http.Request, routerPath string) bool {
	if r == nil || SessionRoleFromContext(r.Context()) != auth.RoleTenantAdmin ||
		OperatorSurfaceAccessFor(r, nil, SurfaceOperatorConsoleRead) != OperatorSurfaceAllowed {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := routerPath
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if OwnerConsoleRefusedPaths[p] {
		return false
	}
	if OwnerConsoleReadablePaths[p] {
		// The control plane's MCP section lists the daemon catalog with its
		// hosts, which the refused MCP pages protect (review fdf7 F3). The
		// handler trims the value (normalizeCPSection), so every spelling
		// that trims to mcp, in any case and in any repeat, is refused
		// (review b8ba F3).
		if p == "/control-plane" {
			for _, v := range r.URL.Query()["section"] {
				if strings.EqualFold(strings.TrimSpace(v), "mcp") {
					return false
				}
			}
		}
		return true
	}
	for prefix, n := range OwnerConsoleDetailPatterns {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		segs := strings.Split(rest, "/")
		if len(segs) != n {
			continue
		}
		valid := true
		for _, s := range segs {
			if s == "" || s == "." || s == ".." || ownerConsoleActionNames[s] || OwnerConsoleReadablePaths[prefix+s] || OwnerConsoleRefusedPaths[prefix+s] {
				valid = false
			}
		}
		if valid {
			return true
		}
	}
	return false
}

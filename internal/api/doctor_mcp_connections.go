package api

import (
	"fmt"
	"time"

	"vornik.io/vornik/internal/mcp"
)

// mcpPendingWarnAfter is how long a project server may be reconnecting
// before the doctor warns (failed-connect recovery design, D4).
const mcpPendingWarnAfter = 2 * time.Minute

// SetMCPPendingSource wires the MCP manager's pending view for the
// mcp_project_connections check. Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetMCPPendingSource(src func() (int, []mcp.PendingServer)) {
	h.mcpPending = src
}

// SetMCPWithheldSource wires the list of project servers withheld at config
// resolution (for example a name-only subscription to a server config.yaml
// does not define). Those are never dialled, so they are not pending.
func (h *DoctorHandlers) SetMCPWithheldSource(src func() []string) {
	h.mcpWithheld = src
}

// checkMCPProjectConnections reports project MCP servers that are
// configured but not connected. It is the source of truth for per-project
// connection state: `vornikctl mcp servers` shows a daemon-level probe and
// said "reachable" through the 2026-09-29 broker outage. Diagnostic only;
// the manager already retries, so there is no --fix.
func (h *DoctorHandlers) checkMCPProjectConnections() DoctorCheck {
	const name = "mcp_project_connections"
	if h.mcpPending == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "MCP manager not wired"}
	}
	now := time.Now().UTC()
	if h.mcpNow != nil {
		now = h.mcpNow().UTC()
	}
	examined, pending := h.mcpPending()
	scope := fmt.Sprintf("%d project servers examined", examined)
	var items []string
	young := 0
	for _, p := range pending {
		if now.Sub(p.Since) < mcpPendingWarnAfter {
			young++
			continue
		}
		items = append(items, fmt.Sprintf("%s/%s: not connected since %s (%d attempts; last error: %s); "+
			"the daemon keeps retrying, so check the server itself",
			p.ProjectID, p.Server, p.Since.UTC().Format(time.RFC3339), p.Attempts, p.LastError))
	}
	if young > 0 {
		scope += fmt.Sprintf("; %d reconnecting for under %s", young, mcpPendingWarnAfter)
	}
	var withheld []string
	if h.mcpWithheld != nil {
		withheld = h.mcpWithheld()
	}
	if len(withheld) > 0 {
		scope += fmt.Sprintf("; %d withheld by configuration", len(withheld))
		for _, w := range withheld {
			items = append(items, w+" (withheld: fix the configuration; the daemon does not retry it)")
		}
	}
	if len(items) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "all project MCP servers connected (" + scope + ")"}
	}
	return DoctorCheck{Name: name, Status: "WARNING",
		Message: fmt.Sprintf("%d project MCP servers are not connected (%s)", len(items), scope),
		Items:   items}
}

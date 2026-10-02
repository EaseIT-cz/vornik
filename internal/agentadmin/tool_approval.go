package agentadmin

import (
	"context"
	"errors"
	"strings"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// ToolApprovalRefusal returns why an agent project may not call MCP tool
// mcp__<server>__<tool>, judged against the device-written approval table
// alone (design §7.3, §10.1), or "" to admit it.
//
// There are no direct writes in a broker project (plan P4.3b): a role's call
// (write=false) is admitted only from the approved READ set of a read entry;
// the worker's call (write=true) only from the approved WRITE set, through
// the "<name>-write" entry. Both resolve the integration with
// agentns.IntegrationOf.
//
// This is the one implementation of that check. Every route that calls an
// agent project's tools asks it: CallMCPTool's gate (plus the role's
// allowlist, which that route alone carries) and the broker write worker
// (review 20261002-a048 F1). The chat dispatcher calls no agent tools at all.
func ToolApprovalRefusal(ctx context.Context, grants persistence.AgentGrantRepository, projectID, tool string, write bool) string {
	if api, method, _, isAPI := (registry.BrokerProposal{Tool: tool}).APITool(); isAPI {
		return apiWriteRefusal(ctx, grants, projectID, api, method, write)
	}
	server, name, ok := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__")
	if !strings.HasPrefix(tool, "mcp__") || !ok || server == "" || name == "" {
		return "the tool is not mcp__<server>__<tool>"
	}
	isWriteEntry := strings.HasSuffix(server, agentns.WriteSuffix)
	switch {
	case write && !isWriteEntry:
		return "a write runs only through the server's write entry"
	case !write && isWriteEntry:
		return "the write entry's tools are proposals only; no role may call them"
	}
	if grants == nil {
		return "the approval tables are not wired"
	}
	a, err := grants.GetIntegration(ctx, projectID, agentns.IntegrationOf(server))
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		return "the server is not approved for this project"
	case err != nil:
		return "the approval could not be read"
	case a.RemovedAt != nil:
		return "the server's approval was removed"
	case a.ReadPending:
		return "the server's tools have not been approved yet"
	}
	set := a.ReadTools
	if write {
		set = a.WriteTools
	}
	for _, t := range set {
		if t == name {
			return ""
		}
	}
	if write {
		return "the tool is not in the server's approved write tools"
	}
	return "the tool is not in the server's approved read tools"
}

// apiWriteRefusal judges an API write proposal (plan P4.8): the worker's
// only, and only for a method in the API's live approved WRITE set.
func apiWriteRefusal(ctx context.Context, grants persistence.AgentGrantRepository, projectID, api, method string, write bool) string {
	if !write {
		return "an API write is a proposal only; no role may make it"
	}
	if grants == nil {
		return "the approval tables are not wired"
	}
	a, err := grants.GetIntegration(ctx, projectID, api)
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		return "the API is not approved for this project"
	case err != nil:
		return "the approval could not be read"
	case a.RemovedAt != nil || a.Kind != "api":
		return "the API's approval was removed"
	}
	for _, m := range a.WriteTools {
		if m == method {
			return ""
		}
	}
	return "the method is not in the API's approved write methods"
}

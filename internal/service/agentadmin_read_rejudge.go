package service

import (
	"context"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
)

// The surfaces of the agent read re-judge (agent-administered design §6.5,
// GitHub #413). The MCP manager judges every call into an agent project's
// read entry against its client's last listing and refuses a tool that now
// changes things or is no longer listed; these say so to the agent
// (list_my_setup), the person (the device's Approvals page) and the
// operator (the doctor check, wired in container_http.go).

// suspendedReads are one agent read entry's suspended tools, from the live
// MCP manager; nil when it has no client.
func (c *Container) suspendedReads(projectID, server string) []string {
	if c == nil || c.mcpManager == nil {
		return nil
	}
	return c.mcpManager.SuspendedReads(projectID, server)
}

// suspendedNote is list_my_setup's clause for suspended tools (§6.5 item 5).
func suspendedNote(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return "the server now describes " + strings.Join(tools, ", ") + " as tools that change things (or no longer lists them), " +
		"so they are suspended and no role can call them; to use them, remove the server and add it again naming them in write_tools"
}

// heldReads are one agent read entry's held tools its current listing
// would let run (amendment #481); nil when it has no client.
func (c *Container) heldReads(projectID, server string) []string {
	if c == nil || c.mcpManager == nil {
		return nil
	}
	return c.mcpManager.HeldReads(projectID, server)
}

// heldNote is list_my_setup's clause for held tools (amendment #481 item 5).
func heldNote(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return "Vornik suspended " + strings.Join(tools, ", ") + " when the server described them as tools that change things; " +
		"they stay suspended even though it no longer says so, until the person approves them again: remove the server and add it again, " +
		"and the person approves that on their device"
}

// joinNotes joins the non-empty notes of one server.
func joinNotes(notes ...string) string {
	var out []string
	for _, n := range notes {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, "; ")
}

// agentWithheldToolLines are the device's lines for every agent read entry
// with suspended tools (§6.5 item 6), in the manager's order.
func (c *Container) agentWithheldToolLines(_ context.Context) []string {
	if c == nil || c.mcpManager == nil {
		return nil
	}
	connected, _ := c.mcpManager.AgentReadVerdicts()
	var out []string
	for _, v := range connected {
		ns, _ := agentns.FromID(v.ProjectID)
		where := "Withheld: " + ns + "'s server " + v.Server + " in " + agentadmin.ProjectSlug(ns, v.ProjectID) + ". "
		if tools := append(append([]string(nil), v.Suspended...), v.Unlisted...); len(tools) > 0 {
			out = append(out, where+"Vornik stopped "+quoteNames(tools)+
				": the server now says they change things (or no longer lists them), so they no longer run as reads. "+
				"Your assistant can ask for them again only as changes you approve one by one.")
		}
		if len(v.Held) > 0 {
			out = append(out, where+"Vornik stopped "+quoteNames(v.Held)+" when the server said they change things. "+
				"They stay stopped, even if the server says otherwise now, until you approve them again: "+
				"your assistant can add the server again, and you approve that here.")
		}
	}
	return out
}

// quoteNames quotes each name: it comes from the server's listing, and a
// server must not be able to make a name read as part of the sentence
// (code review, 2026-10-17).
func quoteNames(tools []string) string {
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = strconv.Quote(t)
	}
	return strings.Join(quoted, ", ")
}

package api

import (
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/mcp"
)

// SetAgentReadVerdicts wires agent_read_tools_rejudged (agent-administered
// design §6.5 item 4, GitHub #413): every connected agent read entry's
// verdict against its client's last listing, and the agent read entries not
// connected. Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetAgentReadVerdicts(fn func() ([]mcp.ReadEntryVerdict, []string)) {
	h.agentReadVerdicts = fn
}

// SetAgentReadHolds wires the held set's store and re-list state into the
// same check (agent-administered design §6.5, amendment #481).
func (h *DoctorHandlers) SetAgentReadHolds(fn func() mcp.ReadHoldState) {
	h.agentReadHolds = fn
}

// checkAgentReadRejudge reports the denominator of the read re-judge: how
// many approved read tools on how many connected agent servers were judged
// against their last listing, how many are suspended (the listing calls
// them mutating) or no longer listed, and how many agent servers were not
// examined because they have no connection. WARNING when anything is
// suspended or unlisted.
func (h *DoctorHandlers) checkAgentReadRejudge() DoctorCheck {
	const name = "agent_read_tools_rejudged"
	if h.agentReadVerdicts == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "MCP manager not wired"}
	}
	connected, notExamined := h.agentReadVerdicts()
	var state *mcp.ReadHoldState
	if h.agentReadHolds != nil {
		st := h.agentReadHolds()
		state = &st
	}
	now := time.Now()
	examined, suspended, unlisted, held := 0, 0, 0, 0
	var items, holdItems []string
	var oldest time.Duration
	for _, v := range connected {
		examined += len(v.Examined)
		suspended += len(v.Suspended)
		unlisted += len(v.Unlisted)
		held += len(v.Held)
		if len(v.Suspended) > 0 {
			items = append(items, fmt.Sprintf("%s/%s suspended: %s", v.ProjectID, v.Server, strings.Join(v.Suspended, ", ")))
		}
		if len(v.Unlisted) > 0 {
			items = append(items, fmt.Sprintf("%s/%s no longer listed: %s", v.ProjectID, v.Server, strings.Join(v.Unlisted, ", ")))
		}
		if len(v.Held) > 0 {
			holdItems = append(holdItems, fmt.Sprintf("%s/%s held: %s", v.ProjectID, v.Server, strings.Join(v.Held, ", ")))
		}
		if !v.ListedAt.IsZero() {
			age := now.Sub(v.ListedAt)
			if age > oldest {
				oldest = age
			}
			if state != nil && state.Interval > 0 && age > 2*state.Interval {
				holdItems = append(holdItems, fmt.Sprintf("%s/%s listing is %s old, over twice the %s re-list interval, so its reads are refused until a re-list succeeds",
					v.ProjectID, v.Server, age.Round(time.Second), state.Interval))
			}
		}
	}
	msg := fmt.Sprintf("%d approved read tool(s) on %d connected agent server(s) examined against their last listing; "+
		"%d suspended; %d no longer listed; %d held from an earlier listing; %d agent server(s) not connected, not examined",
		examined, len(connected), suspended, unlisted, held, len(notExamined))
	if len(notExamined) > 0 {
		msg += " (" + strings.Join(notExamined, ", ") + ")"
	}
	if len(connected) > 0 {
		msg += fmt.Sprintf("; oldest listing %s", oldest.Round(time.Second))
	}
	if state != nil {
		msg += "; " + holdStoreText(*state)
		if !state.Wired {
			holdItems = append(holdItems, "the suspension store is not wired, so a suspension does not survive a restart")
		} else if !state.Loaded {
			holdItems = append(holdItems, "the suspension store has not been read, so every agent read tool is refused until it is")
		}
	}
	if len(items) == 0 && len(holdItems) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: msg}
	}
	if len(items) > 0 {
		msg += ". " + strings.Join(items, "; ") +
			". The server now says these change things, so no role can call them as reads; the agent can add the server again naming them in write_tools, " +
			"or the server can declare readOnlyHint: true for a tool that only reads"
	}
	if len(holdItems) > 0 {
		msg += ". " + strings.Join(holdItems, "; ")
		if held > 0 {
			msg += ". A held tool stays suspended until the person approves it again on their device (the agent removes the server and adds it again)"
		}
	}
	return DoctorCheck{Name: name, Status: "WARNING", Message: msg}
}

// holdStoreText is the store and re-list part of the check's message.
func holdStoreText(st mcp.ReadHoldState) string {
	store := "suspension store not wired"
	switch {
	case st.Wired && st.Loaded:
		store = "suspension store read"
	case st.Wired && st.LoadErr != nil:
		store = "suspension store not read (" + st.LoadErr.Error() + ")"
	case st.Wired:
		store = "suspension store not read yet"
	}
	if st.Interval > 0 {
		return store + fmt.Sprintf("; re-listed every %s", st.Interval)
	}
	return store + "; re-list not running"
}

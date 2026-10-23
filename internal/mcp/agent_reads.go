package mcp

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentns"
)

// The read re-judge of agent projects (agent-administered design §6.5,
// GitHub #413). An agent project's MCP read entry was approved as a set of
// tools Vornik's classifier called read-only when the person approved it
// (§6.4). The server can change its descriptors later, and approvals made
// before #405 were never classified at all. So every call into an agent
// read entry is judged again, here, against the descriptor of the client's
// own last listing: a tool that listing calls mutating, or does not list,
// is refused before anything is sent. The verdict is derived from the
// client, never stored: every listing (daemon start, every reload, every
// re-dial) re-judges, and nothing can disagree with the descriptor the
// call would use.

// ErrReadToolChangesThings refuses an agent project's read call whose tool
// the server now describes as one that changes things, or no longer lists.
// Nothing was sent.
var ErrReadToolChangesThings = errors.New("the tool is suspended: the server now describes it as a tool that changes things")

// agentReadEntry reports whether (projectID, serverName) is an agent
// project's read entry: an agent namespace project, and not its "-write"
// sibling, whose tools are writes by declaration and run only through the
// broker write worker (design §10.1).
func agentReadEntry(projectID, serverName string) bool {
	if _, ok := agentns.FromID(projectID); !ok {
		return false
	}
	return !strings.HasSuffix(serverName, agentns.WriteSuffix)
}

// readVerdicts is a client's verdict on its own listing, built once: the
// listing is immutable for the client's life. mutating holds every listed
// tool IsMutating calls mutating.
func (c *Client) readVerdicts() map[string]bool {
	c.verdictOnce.Do(func() {
		c.mutating = make(map[string]bool, len(c.tools))
		for _, t := range c.tools {
			// A name listed twice is mutating if either descriptor is: a
			// server cannot shadow a destructive entry with a read one.
			c.mutating[t.Name] = c.mutating[t.Name] || t.IsMutating()
		}
	})
	return c.mutating
}

// readRefusal returns why tool may not run as a read on this client, or nil:
// listed, and not mutating by its listed descriptor (fail closed otherwise).
func (c *Client) readRefusal(tool string) error {
	mutating, listed := c.readVerdicts()[tool]
	switch {
	case !listed:
		return fmt.Errorf("%w (MCP server %q no longer lists %s, so nothing says it only reads; it can be approved again only by "+
			"removing the server and adding it again)", ErrReadToolChangesThings, c.config.Name, tool)
	case mutating:
		return fmt.Errorf("%w (MCP server %q now describes %s as a tool that changes things, so it no longer runs as a read; "+
			"it can be approved again only as a write: remove the server and add it again naming it in write_tools)",
			ErrReadToolChangesThings, c.config.Name, tool)
	}
	return nil
}

// suspendedReads are the tools this client's allowlist names that its
// listing calls mutating, sorted, and the allowlisted ones it does not list.
func (c *Client) suspendedReads() (examined, suspended, unlisted []string) {
	v := c.readVerdicts()
	for _, t := range c.tools {
		examined = append(examined, t.Name)
		if v[t.Name] {
			suspended = append(suspended, t.Name)
		}
	}
	for name := range c.allowedSet {
		if _, ok := v[name]; !ok {
			unlisted = append(unlisted, name)
		}
	}
	sort.Strings(examined)
	sort.Strings(suspended)
	sort.Strings(unlisted)
	return examined, suspended, unlisted
}

// agentReadGate is the execution gate's check for one call: nil for
// anything that is not an agent read entry. The client's own verdict on
// its listing (§6.5), then the held set and the listing's age (amendment
// #481).
func (m *Manager) agentReadGate(projectID, serverName, tool string, c *Client) error {
	if !agentReadEntry(projectID, serverName) {
		return nil
	}
	if err := c.readRefusal(tool); err != nil {
		return err
	}
	return m.holdRefusal(projectID, serverName, tool, c)
}

// ReadEntryVerdict is one connected agent read entry's verdict against its
// client's last listing (design §6.5 item 4): the approved tools the
// listing holds (Examined), those it calls mutating (Suspended) and the
// approved ones it no longer lists (Unlisted).
//
// Held (amendment #481) are the approved tools held suspended from an
// earlier listing that this listing would let run: the server no longer
// says they change things, and only a re-approval clears them. ListedAt is
// when this listing was taken.
type ReadEntryVerdict struct {
	ProjectID, Server             string
	Examined, Suspended, Unlisted []string
	Held                          []string
	ListedAt                      time.Time
}

// AgentReadVerdicts returns every connected agent read entry's verdict,
// sorted by project and server, and the configured agent read entries
// with no client ("project/server": pending, so not examined).
func (m *Manager) AgentReadVerdicts() ([]ReadEntryVerdict, []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ReadEntryVerdict
	for projectID, byServer := range m.clients {
		for name, c := range byServer {
			if !agentReadEntry(projectID, name) {
				continue
			}
			v := ReadEntryVerdict{ProjectID: projectID, Server: name, ListedAt: c.listedAt}
			v.Examined, v.Suspended, v.Unlisted = c.suspendedReads()
			v.Held = m.heldListed(projectID, c)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		return out[i].Server < out[j].Server
	})
	var notExamined []string
	for projectID, byServer := range m.pending {
		for name := range byServer {
			if agentReadEntry(projectID, name) && m.clients[projectID][name] == nil {
				notExamined = append(notExamined, projectID+"/"+name)
			}
		}
	}
	for projectID, byServer := range m.withheld {
		for name := range byServer {
			if agentReadEntry(projectID, name) && m.clients[projectID][name] == nil {
				notExamined = append(notExamined, projectID+"/"+name)
			}
		}
	}
	sort.Strings(notExamined)
	return out, notExamined
}

// SuspendedReads returns the tools of one agent read entry its client's
// last listing calls mutating or no longer lists, sorted; nil when the entry
// has no client or is not an agent read entry. Held tools the listing
// would let run are HeldReads.
func (m *Manager) SuspendedReads(projectID, serverName string) []string {
	if !agentReadEntry(projectID, serverName) {
		return nil
	}
	c := m.resolveClient(projectID, serverName)
	if c == nil {
		return nil
	}
	_, suspended, unlisted := c.suspendedReads()
	out := make([]string, 0, len(suspended)+len(unlisted))
	out = append(out, suspended...)
	out = append(out, unlisted...)
	sort.Strings(out)
	return out
}

// logSuspendedReads logs, once per client build, an agent read entry whose
// listing suspends approved tools (design §6.5 item 4).
func (m *Manager) logSuspendedReads(projectID string, c *Client) {
	if c == nil || !agentReadEntry(projectID, c.Name()) {
		return
	}
	_, suspended, unlisted := c.suspendedReads()
	if len(suspended) == 0 && len(unlisted) == 0 {
		return
	}
	m.logger.Warn().Str("project", projectID).Str("server", c.Name()).
		Strs("suspended", suspended).Strs("unlisted", unlisted).
		Msg("mcp: approved read tools of an agent project now change things or are no longer listed; suspended, and refused at every call")
}

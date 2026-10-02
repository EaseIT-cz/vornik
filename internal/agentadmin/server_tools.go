package agentadmin

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/agentns"
)

// VerbApproveServerTools is daemon-internal (plan P4.3, P3 amendment 5): the
// daemon files it once a read-pending server is connected and its tools
// were listed. It is never offered to an agent and the service refuses it
// from a key.
const VerbApproveServerTools = "approve_server_tools"

// ApproveServerToolsInput names the server and what it listed.
type ApproveServerToolsInput struct {
	Project string   `json:"project"` // the full project ID
	Server  string   `json:"server"`
	Tools   []string `json:"tools"`
}

// approveServerTools renders a read-pending server with exactly the listed
// tools allowed. Widening: its grant is the integration with Read = tools
// and read-pending off.
func (r *Renderer) approveServerTools(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbApproveServerTools
	ns := st.Namespace
	var in ApproveServerToolsInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	if got, ok := agentns.FromID(in.Project); !ok || got != ns {
		return refuse(verb, ns, "%q is not a project of %s", in.Project, ns), nil
	}
	p, ok := st.Projects[in.Project]
	if !ok {
		return refuse(verb, ns, "there is no project %q", in.Project), nil
	}
	idx := -1
	for i, s := range p.Servers {
		if s.Name == in.Server {
			idx = i
		}
	}
	if idx < 0 {
		return refuse(verb, ns, "%s has no server %q", in.Project, in.Server), nil
	}
	a, ok := st.Approvals[in.Project][in.Server]
	if !ok || a.Removed || !a.ReadPending {
		return refuse(verb, ns, "the server %q is not waiting for its tools to be approved", in.Server), nil
	}
	listed := dedupSorted(in.Tools)
	if len(listed) == 0 {
		return refuse(verb, ns, "the server listed no tools"), nil
	}
	for _, t := range listed {
		if !toolNameRe.MatchString(t) {
			return refuse(verb, ns, "the server advertises a tool with an unusable name"), nil
		}
	}
	tools, why := splitWrites(listed, a.Write)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	if why := writeEntryDrift(p, in.Server, a.Write); why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	server := p.Servers[idx]
	next := *p
	next.Servers = append([]ServerState(nil), p.Servers...)
	server.Tools = tools
	next.Servers[idx] = server
	yamlOut, err := r.renderProject(ns, &next)
	if err != nil {
		return Change{}, err
	}
	pid := in.Project
	c := Change{
		Ops:     []FileOp{opFor(st, projectPath(pid), yamlOut)},
		ReadSet: map[string]string{},
		Locks:   []string{lockProject(pid), projectPath(pid), lockIntegration(pid, in.Server)},
		Class:   Widening,
		Grant: Grant{Integrations: []IntegrationGrant{{Project: pid, Name: in.Server, Kind: "mcp", URL: server.URL,
			Read: tools, Write: a.Write}}},
	}
	expect(st, c.ReadSet, projectPath(pid))
	c.Sentence = "The server " + in.Server + " at " + hostOf(server.URL) + " is now connected for " + pid +
		" and offers these " + strconv.Itoa(len(tools)) + " tools: " + strings.Join(tools, ", ") +
		". Allow your assistant (" + ns + ") to use them in this project?"
	if len(a.Write) > 0 {
		c.Sentence += " (Its changes, " + strings.Join(a.Write, ", ") + ", stay proposals you approve one by one.)"
	}
	return c, nil
}

func dedupSorted(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// writeEntryDrift reports when the write entry's tools are not the approved
// write set (a hand edit), so the approval never records a write set the
// registry does not declare (review 20261002-5d00 F1).
func writeEntryDrift(p *ProjectState, server string, approved []string) string {
	var declared []string
	for _, s := range p.Servers {
		if s.Name == server+agentns.WriteSuffix {
			declared = dedupSorted(s.Tools)
		}
	}
	if strings.Join(declared, ",") != strings.Join(dedupSorted(approved), ",") {
		return "the server's write entry does not match its approved write tools; remove the integration and add it again"
	}
	return ""
}

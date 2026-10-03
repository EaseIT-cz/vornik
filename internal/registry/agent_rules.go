package registry

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/mcpauth"
)

// Agent-namespace rules at load (agent-administered Vornik design §7.5,
// layer 2; plan P3.7). They catch STRUCTURAL namespace escapes in a file
// that reached the tree by a route other than the renderer (a hand edit, a
// copied file). Each violation is a REJECTED file, so the reload that would
// activate it is refused as a whole and the error names the file. The
// violating object is ALSO removed from the set (review 20261002-a048
// follow-up): a boot load keeps serving what loaded unless
// refuse_start_on_rejected_project is set, and a rejected agent object must
// not be active then either. Tool grants are judged at execution, not here
// (§7.3).

// agentRuleRejections records every violation in cfg's index.
func agentRuleRejections(cfg *ConfigSet, agentModels map[string]bool) {
	if cfg == nil {
		return
	}
	pathOf := map[string]string{}
	if cfg.index != nil {
		for _, src := range cfg.index.Sources {
			if src.ShadowedBy == "" {
				pathOf[src.Kind+"/"+src.ID] = src.Path
			}
		}
	}
	reject := func(kind, id string, err error) {
		path := pathOf[kind+"/"+id]
		if path == "" {
			path = kind + "s/" + id
		}
		cfg.index.reject(kind, path, err)
	}
	for id, p := range cfg.projects {
		if err := checkAgentProject(id, p); err != nil {
			reject("project", id, err)
			delete(cfg.projects, id)
		}
	}
	for id, sw := range cfg.swarms {
		err := checkReservedID(id)
		if err == nil {
			err = checkAgentSwarmModels(id, sw, agentModels)
		}
		if err != nil {
			reject("swarm", id, err)
			delete(cfg.swarms, id)
		}
	}
	for id, wf := range cfg.workflows {
		if err := checkAgentWorkflow(id, wf); err != nil {
			reject("workflow", id, err)
			delete(cfg.workflows, id)
		}
	}
}

// checkReservedID refuses an ID that uses the reserved separator without
// being an agent ID (§5: "--" is reserved; no operator ID may contain it).
func checkReservedID(id string) error {
	if !strings.Contains(id, agentns.Separator) {
		return nil
	}
	if _, ok := agentns.FromID(id); !ok {
		return fmt.Errorf("%q contains %q, which is reserved for agent namespaces, but %q is not a valid agent namespace", id, agentns.Separator, strings.SplitN(id, agentns.Separator, 2)[0])
	}
	return nil
}

// sameNamespace reports whether ref belongs to namespace ns ("" = operator).
func sameNamespace(ns, ref string) bool {
	got, ok := agentns.FromID(ref)
	if !ok {
		got = ""
	}
	return got == ns
}

func checkAgentProject(id string, p *Project) error {
	if err := checkReservedID(id); err != nil {
		return err
	}
	ns, agent := agentns.FromID(id)
	for _, ref := range []string{p.SwarmID, p.DefaultWorkflowID} {
		if ref != "" && !sameNamespace(ns, ref) {
			if agent {
				return fmt.Errorf("agent project %q references %q, outside its namespace %q", id, ref, ns)
			}
			return fmt.Errorf("project %q references %q, which belongs to an agent namespace", id, ref)
		}
	}
	if !agent {
		if len(p.APIs) > 0 {
			return fmt.Errorf("project %q declares apis, which are for agent projects in this release; use gateway.providers", id)
		}
		return nil
	}
	for _, a := range p.APIs {
		if ref := strings.TrimSpace(a.Auth.ValueFrom); ref != "" {
			if err := mcpauth.CheckNamespace([]string{strings.TrimPrefix(ref, "secret://")}, ns); err != nil {
				return fmt.Errorf("agent project %q API %q: %v", id, a.Name, err)
			}
		}
	}
	if !p.Broker {
		return fmt.Errorf("agent project %q must be a broker project (broker: true)", id)
	}
	if err := checkAgentWritePairs(id, p.MCP.Servers); err != nil {
		return err
	}
	for _, s := range p.MCP.Servers {
		if strings.Contains(s.Name, "__") {
			return fmt.Errorf("agent project %q server %q contains %q, the tool separator of mcp__<server>__<tool>", id, s.Name, "__")
		}
		if strings.TrimSpace(s.Command) != "" {
			return fmt.Errorf("agent project %q declares a server %q that runs a program; agent servers are remote only", id, s.Name)
		}
		if ref := strings.TrimSpace(s.Auth.ValueFrom); ref != "" {
			if err := mcpauth.CheckNamespace([]string{strings.TrimPrefix(ref, "secret://")}, ns); err != nil {
				return fmt.Errorf("agent project %q server %q: %v", id, s.Name, err)
			}
		}
	}
	return nil
}

func checkAgentWorkflow(id string, wf *Workflow) error {
	if err := checkReservedID(id); err != nil {
		return err
	}
	ns, agent := agentns.FromID(id)
	if !agent {
		return checkOperatorWorkflowReach(id, wf)
	}
	for name, st := range wf.Steps {
		// The renderer writes agent steps only. Any other step type can
		// reach code outside the declared role (a handler, another project,
		// a delegated workflow), so it is refused here.
		if st.Type != "agent" || st.Handler != "" || st.DelegatedWorkflow != "" {
			return fmt.Errorf("agent workflow %q step %q is not a plain agent step", id, name)
		}
	}
	// Publishing as an A2A agent would be a way out that is not the
	// approved broker egress.
	if wf.A2A.Publish || len(wf.A2A.Projects) > 0 {
		return fmt.Errorf("agent workflow %q may not be published as an A2A agent", id)
	}
	for _, ref := range workflowRefs(wf) {
		if !sameNamespace(ns, ref) {
			return fmt.Errorf("agent workflow %q references %q, outside its namespace %q", id, ref, ns)
		}
	}
	return nil
}

// checkOperatorWorkflowReach refuses an operator workflow that names an
// agent project or workflow (review 20261002-a048 F5). Delegated subtasks and
// parallel branches run in the CALLER's project, so an agent workflow would
// run outside its broker project with the caller's tools. A call_project
// into an agent project would hand the agent's approved integrations a task
// nobody in that namespace asked for.
func checkOperatorWorkflowReach(id string, wf *Workflow) error {
	// Schedules are for agent workflows in this release (agent-administered
	// Vornik design §17.1); operators have reminders and autonomy.
	if wf.Broker != nil && wf.Broker.Schedule != nil {
		return fmt.Errorf("workflow %q declares broker.schedule; schedules are for agent workflows in this release", id)
	}
	for _, ref := range workflowRefs(wf) {
		if _, agent := agentns.FromID(ref); agent {
			return fmt.Errorf("workflow %q reaches into agent namespace with %q; agent workflows run only inside their own projects", id, ref)
		}
	}
	return nil
}

// workflowRefs lists every project or workflow ID wf's steps name.
func workflowRefs(wf *Workflow) []string {
	var refs []string
	for _, st := range wf.Steps {
		for _, r := range []string{st.DelegatedWorkflow, st.TargetProject, st.TargetWorkflow} {
			if r != "" {
				refs = append(refs, r)
			}
		}
		for _, b := range st.Branches {
			if b.Workflow != "" {
				refs = append(refs, b.Workflow)
			}
		}
	}
	return refs
}

// checkAgentWritePairs enforces the write-entry shape (plan P4.3b): a
// broker_write server is named "<base>-write", and its base is a read entry
// of the same URL and credential. Otherwise a hand edit could point the
// write side, which reuses the base's approval, at another host.
func checkAgentWritePairs(id string, servers []MCPServerConfig) error {
	byName := make(map[string]MCPServerConfig, len(servers))
	for _, s := range servers {
		byName[s.Name] = s
	}
	for _, s := range servers {
		isWriteName := strings.HasSuffix(s.Name, agentns.WriteSuffix)
		if s.BrokerWrite != isWriteName {
			return fmt.Errorf("agent project %q server %q: a write entry is broker_write and named <name>%s, and only a write entry is", id, s.Name, agentns.WriteSuffix)
		}
		if !isWriteName {
			continue
		}
		base, ok := byName[agentns.IntegrationOf(s.Name)]
		if !ok || base.BrokerWrite || base.URL != s.URL || base.Auth.ValueFrom != s.Auth.ValueFrom {
			return fmt.Errorf("agent project %q server %q: a write entry needs its read entry %q with the same URL and credential", id, s.Name, agentns.IntegrationOf(s.Name))
		}
	}
	return nil
}

// checkAgentSwarmModels applies agent-administered design §18.6 item 2 to an
// agent namespace's swarm: no role carries a modelFallback (Change 7, review
// d94f F6), and a role's model is in the operator's catalogue (round 2 F4).
// Destination approval is not judged here: it lives in the database and is
// checked before every attempt at run time (round 3 F5). agentModels nil
// skips the membership check (a registry the daemon did not configure).
func checkAgentSwarmModels(id string, sw *Swarm, agentModels map[string]bool) error {
	if sw == nil {
		return nil
	}
	if _, agent := agentns.FromID(id); !agent {
		return nil
	}
	for _, r := range sw.Roles {
		if strings.TrimSpace(r.ModelFallback) != "" {
			return fmt.Errorf("agent swarm %q role %q declares a modelFallback; an agent role runs only on its own model", id, r.Name)
		}
		if m := strings.TrimSpace(r.Model); m != "" && !agentModels[m] {
			return fmt.Errorf("agent swarm %q role %q names the model %q, which is not in the operator's catalogue (agent_admin.models)", id, r.Name, m)
		}
	}
	return nil
}

package agentadmin

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/registry"
)

const (
	maxRoles        = 8
	maxRoleTools    = 32
	maxSteps        = 10
	maxSchemaBytes  = 8 << 10
	maxEgressBytes  = 16 << 10
	egressDocBytes  = 16384
	maxBudgetUSD    = 1000
	placeholderStep = "start"
	// maxWorkflowIDLen is the workflow name limit the workflow validator
	// enforces (registry workflowMDNameMaxLen).
	maxWorkflowIDLen = 64
)

// defaultRole is the one role a new project's swarm starts with: workspace
// and clock tools only.
func defaultRole() RoleSpec {
	return RoleSpec{
		Name:         "worker",
		Description:  "Does one step using only the files in its workspace",
		Instructions: "Do the step you are given, using only the files in your workspace.",
		Tools:        []string{"current_time", "file_read", "file_write"},
	}
}

// WorkflowID is an agent workflow's ID: <ns>--<project>--<workflow>. Slugs
// never contain "--", so the owning project is structural, not recorded
// anywhere that could drift (ProjectOfWorkflow).
func WorkflowID(ns, projectSlug, workflowSlug string) string {
	return agentns.ID(ns, projectSlug+agentns.Separator+workflowSlug)
}

// ProjectOfWorkflow returns the project ID owning an agent workflow ID, or
// "" when the ID is not of that shape.
func ProjectOfWorkflow(workflowID string) string {
	ns, ok := agentns.FromID(workflowID)
	if !ok {
		return ""
	}
	rest := strings.TrimPrefix(workflowID, ns+agentns.Separator)
	project, wf, found := strings.Cut(rest, agentns.Separator)
	if !found || project == "" || wf == "" {
		return ""
	}
	return agentns.ID(ns, project)
}

func lockProject(id string) string             { return "project:" + id }
func lockWorkflow(id string) string            { return "workflow:" + id }
func lockIntegration(project, n string) string { return "integration:" + project + "/" + n }

func (r *Renderer) createProject(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbCreateProject
	ns := st.Namespace
	var in CreateProjectInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	if err := checkSlug("slug", in.Slug); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	if err := checkOneLine("purpose", in.Purpose, true); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	if in.Template != "" && in.Template != "default" {
		return refuse(verb, ns, "template %q does not exist; the only template is \"default\"", in.Template), nil
	}
	id := agentns.ID(ns, in.Slug)
	wfID := WorkflowID(ns, in.Slug, placeholderStep)
	if len(wfID) > maxWorkflowIDLen {
		return refuse(verb, ns, "the slug %q is too long for this namespace; use at most %d characters", in.Slug, maxWorkflowIDLen-len(wfID)+len(in.Slug)), nil
	}
	if _, ok := st.Projects[id]; ok {
		return refuse(verb, ns, "the project %q already exists", id), nil
	}
	for _, path := range []string{projectPath(id), swarmPath(id), workflowPath(wfID)} {
		if _, ok := st.FileHashes[path]; ok {
			return refuse(verb, ns, "%s already exists", path), nil
		}
	}
	p := &ProjectState{
		ID: id, DisplayName: in.Slug, Purpose: in.Purpose, MonthlyUSD: st.DefaultBudgetUSD,
		DefaultWorkflowID: wfID, Swarm: SwarmState{ID: id, Roles: []RoleSpec{defaultRole()}},
	}
	projectYAML, err := r.renderProject(ns, p)
	if err != nil {
		return Change{}, err
	}
	swarmMD, err := r.renderSwarm(st, p)
	if err != nil {
		return Change{}, err
	}
	wfMD, err := r.exec(tmplWorkflow, placeholderWorkflow(ns, wfID))
	if err != nil {
		return Change{}, err
	}
	c := Change{
		Ops: []FileOp{
			{Op: OpCreate, Path: projectPath(id), Content: projectYAML},
			{Op: OpCreate, Path: swarmPath(id), Content: swarmMD},
			{Op: OpCreate, Path: workflowPath(wfID), Content: wfMD},
		},
		ReadSet: map[string]string{},
		Locks:   []string{lockProject(id), lockWorkflow(wfID), projectPath(id), swarmPath(id), workflowPath(wfID)},
		Class:   Inert,
	}
	for _, op := range c.Ops {
		c.ReadSet[op.Path] = ReadSetAbsent
	}
	c.Sentence = fmt.Sprintf("Your assistant (%s) created the project %q (%s) with a $%s monthly budget.",
		ns, id, quoteShort(in.Purpose), formatUSD(p.MonthlyUSD))
	var g Grant
	if clause, raises := spendingTerms(st, p.MonthlyUSD, &g); raises {
		// Within the ceiling the change is inert and applies now; no grant.
		c.Class = Widening
		c.Grant.AddsUSD, c.Grant.MaxTotalUSD = g.AddsUSD, g.MaxTotalUSD
		c.Sentence = fmt.Sprintf("Your assistant (%s) wants to create the project %q (%s) with a $%s monthly budget.",
			ns, id, quoteShort(in.Purpose), formatUSD(p.MonthlyUSD)) + clause
	}
	return c, nil
}

// placeholderWorkflow is a new project's default workflow: no integrations,
// no inputs, and an egress of one yes/no. It never runs unless approved.
func placeholderWorkflow(ns, id string) workflowData {
	return workflowData{
		Namespace: ns, ID: id, DisplayName: "Start", Description: "Placeholder default workflow",
		Entrypoint:   placeholderStep,
		InputSchema:  `{"additionalProperties":false,"properties":{},"type":"object"}`,
		EgressSchema: `{"additionalProperties":false,"properties":{"ok":{"type":"boolean"}},"required":["ok"],"type":"object"}`,
		MaxBytes:     egressDocBytes,
		Steps: []stepData{{Name: placeholderStep, Role: defaultRole().Name, Next: "done", First: true, Last: true, Closing: closingOneStep,
			Instructions: "Report whether you could start: write ok as true."}},
	}
}

type stepData struct {
	Name, Role, Instructions, Next string
	// RequireOutputGlob is rendered onto non-answer steps so the executor
	// enforces the same hand-off file the prompt tells the step to write.
	RequireOutputGlob string
	// First and Last place the step in the chain (design §18.10): every step
	// but the first is told where earlier steps' files are, every step but
	// the last how to hand work on, and the last writes the answer.
	First, Last bool
	// Closing ends the last step's text: today's sentence for a one-step
	// workflow, and for a longer one a sentence that leaves earlier steps'
	// files alone.
	Closing string
}

const (
	closingOneStep   = "Write no other output file."
	closingMultiStep = "Write no other file, and do not change earlier steps' files."
)

type workflowData struct {
	Namespace, ID, DisplayName, Description, Entrypoint string
	InputSchema, EgressSchema                           string
	MaxBytes                                            int
	Steps                                               []stepData
	Proposes                                            []proposeData
	// Schedule is broker.schedule as a JSON flow mapping; "" for none.
	Schedule string
	// Version is the workflow's version; "" renders 1.0.0. A recipe install
	// marks its recipe in the build metadata (recipeVersionTag).
	Version string
}

func (r *Renderer) defineSwarm(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbDefineSwarm
	ns := st.Namespace
	var in DefineSwarmInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	in.Slug = projectSlug(ns, in.Slug) // names an existing project (design section 5, 2026-10-07)
	id := agentns.ID(ns, in.Slug)
	p, ok := st.Projects[id]
	if !ok {
		return refuse(verb, ns, "there is no project %q; a project's roles are defined under the project's own slug", id), nil
	}
	roles, why := buildRoles(st, p, in.Roles)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	next := *p
	next.Swarm = SwarmState{ID: p.Swarm.ID, Roles: roles}
	if next.Swarm.ID == "" {
		next.Swarm.ID = id
	}
	if why := missingWorkflowRole(st, id, roles); why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	swarmMD, err := r.renderSwarm(st, &next)
	if err != nil {
		return Change{}, err
	}
	loadedSwarm, err := registry.ParseSwarmMarkdown([]byte(swarmMD), swarmPath(next.Swarm.ID))
	if err != nil {
		return Change{}, fmt.Errorf("rendered swarm does not parse: %w", err)
	}
	c := Change{
		Ops:     []FileOp{opFor(st, swarmPath(next.Swarm.ID), swarmMD)},
		ReadSet: map[string]string{},
		Locks:   []string{lockProject(id), swarmPath(next.Swarm.ID)},
		Class:   Inert,
	}
	expect(st, c.ReadSet, swarmPath(next.Swarm.ID))
	expect(st, c.ReadSet, projectPath(id))
	c.Sentence = fmt.Sprintf("Your assistant (%s) changed the roles of %q.", ns, id)
	changed, err := rebindReach(st, p, loadedSwarm, &c)
	if err != nil {
		return Change{}, err
	}
	modelClauses := modelReach(st, roles, &c)
	if len(changed) > 0 || len(modelClauses) > 0 {
		c.Class = Widening
		c.Sentence = fmt.Sprintf("Your assistant (%s) wants to change the roles of %q.", ns, id)
		for _, m := range modelClauses {
			c.Sentence += " " + m
		}
		if len(changed) > 0 {
			c.Sentence += fmt.Sprintf(" The workflow %s.", strings.Join(changed, "; the workflow "))
		}
	}
	return c, nil
}

// catalogueModel checks a role's model against the operator's catalogue
// (design §18.6 item 2; §7.1's Refused row: a model outside it).
func catalogueModel(st *State, ri RoleInput) (string, string) {
	model := strings.TrimSpace(ri.Model)
	if model == "" {
		return "", ""
	}
	if len(st.models) == 0 {
		return "", "no model can be chosen on this installation: the operator lists none (agent_admin.models); leave model out to use the installation's default"
	}
	if _, ok := st.models[model]; !ok {
		ids := make([]string, 0, len(st.models))
		for _, m := range SortedModels(st.models) {
			ids = append(ids, m.ID)
		}
		return "", fmt.Sprintf("the model %q is not in the installation's catalogue; choose one of: %s (describe_installation says what each is good for)", model, strings.Join(ids, ", "))
	}
	return model, ""
}

// modelReach makes a role on a remote model whose destination the namespace
// has not approved a widening of the change (design §18.6 item 2, rounds 2
// and 3): the approval records the destination, "<sub-provider>@<host>",
// for the whole namespace, so a second role on it applies at once. It
// returns one sentence clause per such role, naming the host.
func modelReach(st *State, roles []RoleSpec, c *Change) []string {
	var clauses []string
	granted := map[string]bool{}
	for _, r := range roles {
		if r.Model == "" {
			continue
		}
		m, ok := st.models[r.Model]
		if !ok || m.Dest.Local || st.ApprovedDestinations[m.Dest.String()] {
			continue
		}
		dest := m.Dest.String()
		if !granted[dest] {
			granted[dest] = true
			c.Grant.Models = append(c.Grant.Models, ModelGrant{Destination: dest, Model: r.Model, Role: r.Name})
		}
		clauses = append(clauses, fmt.Sprintf("The %s role would use the model %q, which sends what the %s role works on to %s.", r.Name, r.Model, r.Name, m.Dest.Words()))
	}
	return clauses
}

// buildRoles validates define_swarm's roles and returns them, with the
// worker role kept: every project's placeholder default workflow runs it,
// and the loader drops a project whose workflow names a missing role.
func buildRoles(st *State, p *ProjectState, in []RoleInput) ([]RoleSpec, string) {
	if len(in) == 0 || len(in) > maxRoles {
		return nil, fmt.Sprintf("give between 1 and %d roles", maxRoles)
	}
	seen := map[string]bool{}
	var roles []RoleSpec
	for _, ri := range in {
		if !nameRe.MatchString(ri.Name) || seen[ri.Name] {
			return nil, fmt.Sprintf("role name %q must be unique and a-z, 0-9, _ or -", ri.Name)
		}
		seen[ri.Name] = true
		if err := checkText("role "+ri.Name+" instructions", ri.Instructions, maxTextRunes, true); err != nil {
			return nil, err.Error()
		}
		if len(ri.Tools) == 0 || len(ri.Tools) > maxRoleTools {
			return nil, fmt.Sprintf("role %s needs between 1 and %d tools (a role with no tool list could use every tool)", ri.Name, maxRoleTools)
		}
		tools, why := allowedRoleTools(st, p, ri.Tools)
		if why != "" {
			return nil, "role " + ri.Name + ": " + why
		}
		// A broker step returns only through artifacts/out/result.json, so a
		// role without file_write can never answer (§18.1). It is a
		// workspace-confined built-in: granting it widens nothing.
		if !contains(tools, "file_write") {
			tools = append(tools, "file_write")
			sort.Strings(tools)
		}
		model, why := catalogueModel(st, ri)
		if why != "" {
			return nil, "role " + ri.Name + ": " + why
		}
		roles = append(roles, RoleSpec{Name: ri.Name, Description: firstLine(ri.Instructions), Instructions: ri.Instructions, Tools: tools, Model: model})
	}
	if !seen[defaultRole().Name] {
		roles = append(roles, defaultRole())
	}
	return roles, ""
}

// missingWorkflowRole refuses a role set that drops a role some workflow of
// the project still runs.
func missingWorkflowRole(st *State, project string, roles []RoleSpec) string {
	have := map[string]bool{}
	for _, r := range roles {
		have[r.Name] = true
	}
	for _, wid := range st.workflowsOf(project) {
		w := st.Workflows[wid]
		if w.Loaded == nil {
			continue
		}
		for name, step := range w.Loaded.Steps {
			if step.Role != "" && !have[step.Role] {
				return fmt.Sprintf("workflow %s step %s uses the role %q, which this change removes", wid, name, step.Role)
			}
		}
	}
	return ""
}

// rebindReach recomputes the reach of the project's approved workflows under
// the new swarm. A workflow whose reach would change is re-bound by this
// change's approval, so the change is widening.
func rebindReach(st *State, p *ProjectState, sw *registry.Swarm, c *Change) ([]string, error) {
	var changed []string
	for _, wid := range st.workflowsOf(p.ID) {
		w := st.Workflows[wid]
		if w.Loaded == nil || w.ApprovedReach == "" {
			continue
		}
		sig, err := SignatureOf(p.Loaded, sw, w.Loaded)
		if err != nil {
			return nil, err
		}
		if h := sig.Hash(); h != w.ApprovedReach {
			if c.Grant.Workflows == nil {
				c.Grant.Workflows = map[string]string{}
			}
			c.Grant.Workflows[wid] = h
			c.Locks = append(c.Locks, lockWorkflow(wid))
			changed = append(changed, fmt.Sprintf("%q would then reach %s", wid, reachPhrase(sig)))
		}
	}
	return changed, nil
}

// allowedRoleTools applies §7.3: a no-egress built-in, or a read tool of an
// integration approved for this project. Write tools are never held by a
// role (the daemon calls them after a person approves a draft).
func allowedRoleTools(st *State, p *ProjectState, tools []string) ([]string, string) {
	out := make([]string, 0, len(tools))
	seen := map[string]bool{}
	for _, t := range tools {
		t = strings.TrimSpace(t)
		if seen[t] {
			continue
		}
		seen[t] = true
		if registry.IsBrokerSafeBuiltin(t) {
			out = append(out, t)
			continue
		}
		if t == QueryAPITool {
			// A reader of the project's approved APIs (plan P4.5): GET and
			// HEAD only; writes are proposals.
			if len(liveAPIs(st, p)) == 0 {
				return nil, fmt.Sprintf("%s needs an approved API in %s; add one with add_api first", QueryAPITool, p.ID)
			}
			out = append(out, t)
			continue
		}
		srv, tool, ok := splitMCPTool(t)
		if !ok {
			return nil, fmt.Sprintf("%q is not an allowed tool (allowed: %s, or mcp__<server>__<tool> of an approved server)", t, strings.Join(registry.BrokerSafeBuiltins(), ", "))
		}
		a, approved := st.Approvals[p.ID][srv]
		if !approved || !a.Live() {
			return nil, fmt.Sprintf("the server %q is not approved for %s; add it with add_mcp_server first", srv, p.ID)
		}
		if a.ReadPending {
			return nil, fmt.Sprintf("the tools of %q have not been approved yet", srv)
		}
		if !contains(a.Read, tool) {
			return nil, fmt.Sprintf("%q is not one of the approved read tools of %q (%s)", tool, srv, strings.Join(a.Read, ", "))
		}
		out = append(out, t)
	}
	sort.Strings(out)
	return out, ""
}

func (r *Renderer) defineWorkflow(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbDefineWorkflow
	ns := st.Namespace
	var in DefineWorkflowInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	in.Project = projectSlug(ns, in.Project)
	pid := agentns.ID(ns, in.Project)
	p, ok := st.Projects[pid]
	if !ok {
		return refuse(verb, ns, "there is no project %q", pid), nil
	}
	if p.Loaded == nil || p.LoadedSwarm == nil {
		return refuse(verb, ns, "the project %q is not loaded yet; try again in a moment", pid), nil
	}
	if why := checkWorkflowFields(in); why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	id := WorkflowID(ns, in.Project, in.Slug)
	if len(id) > maxWorkflowIDLen {
		return refuse(verb, ns, "the workflow ID %q is %d characters; use shorter project and workflow slugs (the limit is %d)", id, len(id), maxWorkflowIDLen), nil
	}
	steps, why := buildSteps(p, in.Steps)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	inputSchema, err := checkInputSchema(in.Inputs)
	if err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	if len(in.Egress) > maxEgressBytes {
		return refuse(verb, ns, "the egress schema is over %d bytes", maxEgressBytes), nil
	}
	egressSchema, egressLines, err := checkEgressSchema(in.Egress)
	if err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	egressJSON, err := canonicalOf(egressSchema)
	if err != nil {
		return Change{}, err
	}
	proposes, proposePhrases, why := parseProposes(st, p, in.Proposes)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	schedule, why := renderSchedule(in.Schedule)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	data := workflowData{
		Namespace: ns, ID: id, DisplayName: in.Slug, Description: describe(in.Purpose, "Workflow "+in.Slug+" of "+pid), Entrypoint: steps[0].Name,
		InputSchema: inputSchema, EgressSchema: string(egressJSON), MaxBytes: egressDocBytes, Steps: steps, Proposes: proposes,
		Schedule: schedule,
	}
	md, err := r.exec(tmplWorkflow, data)
	if err != nil {
		return Change{}, err
	}
	wf, why := parseRenderedWorkflow(md, id)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	sig, err := SignatureOf(p.Loaded, p.LoadedSwarm, wf)
	if err != nil {
		return Change{}, err
	}
	return workflowChange(st, p, wf, md, sig, egressLines, proposePhrases), nil
}

// workflowChange classifies a rendered workflow: inert when its reach
// signature equals the approved one, widening (with the approval sentence
// and the grant that re-binds it) otherwise.
func workflowChange(st *State, p *ProjectState, wf *registry.Workflow, md string, sig ReachSignature, egressLines, proposePhrases []string) Change {
	ns, id, pid := st.Namespace, wf.ID, p.ID
	hash := sig.Hash()
	c := Change{
		Ops:     []FileOp{opFor(st, workflowPath(id), md)},
		ReadSet: map[string]string{},
		Locks:   []string{lockProject(pid), lockWorkflow(id), workflowPath(id)},
		Class:   Inert,
		reach:   &sig,
	}
	expect(st, c.ReadSet, workflowPath(id))
	expect(st, c.ReadSet, projectPath(pid))
	expect(st, c.ReadSet, swarmPath(p.Swarm.ID))
	c.Sentence = fmt.Sprintf("Your assistant (%s) updated the steps of the workflow %q; what it returns and what it can reach are unchanged.", ns, id)
	if w, ok := st.Workflows[id]; !ok || w.ApprovedReach != hash {
		c.Class = Widening
		c.Grant.Workflows = map[string]string{id: hash}
		c.Sentence = fmt.Sprintf("Your assistant (%s) wants the project %q to run the workflow %q. "+
			"It will return to your assistant: %s. It can reach %s.",
			ns, pid, id, strings.Join(egressLines, "; "), reachPhrase(sig))
		c.Sentence += documentSentence(sig)
		c.Sentence += proposeSentence(proposePhrases)
		c.Sentence += scheduleSentence(id, wf.Broker.Schedule, st.Workflows[id])
	}
	return c
}

// checkWorkflowFields refuses define_workflow's scalar fields.
func checkWorkflowFields(in DefineWorkflowInput) string {
	if err := checkSlug("slug", in.Slug); err != nil {
		return err.Error()
	}
	if err := checkOneLine("purpose", in.Purpose, false); err != nil {
		return err.Error()
	}
	return ""
}

// buildSteps validates the steps and links them in order, the last to done.
func buildSteps(p *ProjectState, in []StepInput) ([]stepData, string) {
	if len(in) == 0 || len(in) > maxSteps {
		return nil, fmt.Sprintf("give between 1 and %d steps", maxSteps)
	}
	roles := map[string]bool{}
	for _, rl := range p.LoadedSwarm.Roles {
		roles[rl.Name] = true
	}
	var steps []stepData
	seen := map[string]bool{"done": true}
	for i, s := range in {
		if !nameRe.MatchString(s.Name) || seen[s.Name] {
			return nil, fmt.Sprintf("step name %q must be unique, not \"done\", and a-z, 0-9, _ or -", s.Name)
		}
		seen[s.Name] = true
		if !roles[s.Role] {
			return nil, fmt.Sprintf("step %s uses the role %q, which %s does not have", s.Name, s.Role, p.ID)
		}
		if err := checkText("step "+s.Name+" instructions", s.Instructions, maxTextRunes, true); err != nil {
			return nil, err.Error()
		}
		next := "done"
		if i+1 < len(in) {
			next = in[i+1].Name
		}
		closing := closingOneStep
		if len(in) > 1 {
			closing = closingMultiStep
		}
		last := i+1 == len(in)
		requireOutputGlob := ""
		if !last {
			requireOutputGlob = "artifacts/out/" + s.Name + ".md"
			if other := conflictingOutputPath(s.Instructions, requireOutputGlob); other != "" {
				return nil, fmt.Sprintf("step %s instructions name %q, but non-answer steps must write their hand-off to %q", s.Name, other, requireOutputGlob)
			}
		}
		steps = append(steps, stepData{Name: s.Name, Role: s.Role, Instructions: s.Instructions, Next: next,
			RequireOutputGlob: requireOutputGlob, First: i == 0, Last: last, Closing: closing})
	}
	return steps, ""
}

func conflictingOutputPath(instructions, expected string) string {
	for _, loc := range outPathRe.FindAllStringIndex(instructions, -1) {
		p := instructions[loc[0]:loc[1]]
		if p != expected {
			before := instructions[:loc[0]]
			if len(before) > 80 {
				before = before[len(before)-80:]
			}
			if outIntentRe.MatchString(before) {
				return p
			}
		}
	}
	return ""
}

// checkInputSchema accepts an object schema the broker compiler accepts, and
// returns it as canonical JSON. An absent schema means "no inputs".
func checkInputSchema(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)
	}
	if len(raw) > maxSchemaBytes {
		return "", fmt.Errorf("the inputs schema is over %d bytes", maxSchemaBytes)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil || m["type"] != "object" {
		return "", fmt.Errorf("inputs must be a JSON Schema object (\"type\": \"object\")")
	}
	if _, err := registry.CompileBrokerSchema("inputs", m); err != nil {
		return "", fmt.Errorf("the inputs schema does not compile: %v", err)
	}
	canon, err := canonicalOf(m)
	if err != nil {
		return "", err
	}
	return string(canon), nil
}

func (r *Renderer) addMCPServer(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbAddMCPServer
	ns := st.Namespace
	var in AddMCPServerInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	in.Project = projectSlug(ns, in.Project)
	pid := agentns.ID(ns, in.Project)
	p, ok := st.Projects[pid]
	if !ok {
		return refuse(verb, ns, "there is no project %q", pid), nil
	}
	if !nameRe.MatchString(in.Name) || strings.Contains(in.Name, "__") || strings.HasSuffix(in.Name, agentns.WriteSuffix) {
		return refuse(verb, ns, "server name %q must be a-z, 0-9, _ or - (no double underscore, not ending in %s)", in.Name, agentns.WriteSuffix), nil
	}
	if err := checkURL(in.URL); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	writes := dedupSorted(in.WriteTools)
	if len(writes) > 16 {
		return refuse(verb, ns, "at most 16 write tools"), nil
	}
	for _, t := range writes {
		if !toolNameRe.MatchString(t) {
			return refuse(verb, ns, "write tool %q is not a tool name", t), nil
		}
	}
	for _, s := range p.Servers {
		if agentns.IntegrationOf(s.Name) == in.Name {
			return refuse(verb, ns, "%s already has a server named %q; remove it first", pid, in.Name), nil
		}
	}
	server := ServerState{Name: in.Name, URL: in.URL}
	if in.Auth.Mode == "oauth" {
		server.OAuth, server.Scopes = true, dedupSorted(in.Auth.Scopes)
	}
	credential, why := mcpCredential(in.Auth)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	if credential != "" {
		server.AuthRef = "secret://" + ns + "/" + credential
	}
	grant := IntegrationGrant{Project: pid, Name: in.Name, Kind: "mcp", URL: in.URL, Write: writes}
	if why := grantAdvertised(st, in.URL, writes, &grant); why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	server.Tools = grant.Read
	next := *p
	next.Servers = append(append([]ServerState(nil), p.Servers...), server)
	if len(writes) > 0 {
		// The broker_write sibling (P4.3b): no role may hold its tools; they
		// are reachable only as a workflow's proposes, one tap per write.
		next.Servers = append(next.Servers, ServerState{Name: in.Name + agentns.WriteSuffix, URL: in.URL,
			AuthRef: server.AuthRef, OAuth: server.OAuth, Scopes: server.Scopes, Write: true, Tools: writes})
	}
	if credential != "" && !contains(p.Secrets, ns+"/"+credential) {
		next.Secrets = append(append([]string(nil), p.Secrets...), ns+"/"+credential)
	}
	yamlOut, err := r.renderProject(ns, &next)
	if err != nil {
		return Change{}, err
	}
	c := Change{
		Ops:     []FileOp{opFor(st, projectPath(pid), yamlOut)},
		ReadSet: map[string]string{},
		Locks:   []string{lockProject(pid), projectPath(pid), lockIntegration(pid, in.Name)},
		Class:   Widening,
		Grant:   Grant{Integrations: []IntegrationGrant{grant}},
	}
	expect(st, c.ReadSet, projectPath(pid))
	c.Sentence = serverSentence(st, ns, pid, in, credential, grant)
	return c, nil
}

// mcpCredential returns the credential NAME an auth block binds, or why it
// is refused.
func mcpCredential(a MCPAuthInput) (string, string) {
	switch a.Mode {
	case "", "none":
		return "", ""
	case "static":
		if !credentialRe.MatchString(a.Credential) {
			return "", fmt.Sprintf("credential %q must be A-Z, 0-9 or _ and start with a letter", a.Credential)
		}
		return a.Credential, ""
	case "oauth":
		if a.Credential != "" {
			return "", "an OAuth server takes no credential name; its token is OAUTH_<SERVER>, filled by signing in"
		}
		if len(a.Scopes) > 20 {
			return "", "at most 20 scopes"
		}
		for _, sc := range a.Scopes {
			if !scopeRe.MatchString(sc) {
				return "", fmt.Sprintf("scope %q is not a scope", sc)
			}
		}
		return "", ""
	default:
		return "", fmt.Sprintf("auth mode %q must be none, static or oauth", a.Mode)
	}
}

// serverSentence states an add_mcp_server change, with the history of a
// removed approval of the same name (plan amendment 2).
func serverSentence(st *State, ns, pid string, in AddMCPServerInput, credential string, grant IntegrationGrant) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your assistant (%s) wants the project %q to connect to the server %q at %s", ns, pid, in.Name, in.URL)
	if credential != "" {
		fmt.Fprintf(&b, " using the credential %s", credential)
	}
	if in.Auth.Mode == "oauth" {
		fmt.Fprintf(&b, ", signing in with your account there (OAuth; asking for: %s)", listOrNone(dedupSorted(in.Auth.Scopes)))
	}
	b.WriteString(". ")
	if grant.ReadPending {
		b.WriteString("The server did not list its tools without signing in; you will be asked to approve its tools separately once it is connected.")
	} else {
		fmt.Fprintf(&b, "It would be able to use these %d read tools: %s.", len(grant.Read), strings.Join(grant.Read, ", "))
	}
	if len(grant.Write) > 0 {
		fmt.Fprintf(&b, " It could also propose changes with: %s. Each change will be shown to you for approval before it is made.", strings.Join(grant.Write, ", "))
	}
	if prev, ok := st.Approvals[pid][in.Name]; ok && prev.Removed {
		fmt.Fprintf(&b, " Previously approved and then removed: %s at %s, read tools %s.", in.Name, prev.URL, listOrNone(prev.Read))
	}
	return b.String()
}

func (r *Renderer) setBudget(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbSetBudget
	ns := st.Namespace
	var in SetBudgetInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	in.Project = projectSlug(ns, in.Project)
	pid := agentns.ID(ns, in.Project)
	p, ok := st.Projects[pid]
	if !ok {
		return refuse(verb, ns, "there is no project %q", pid), nil
	}
	v := in.MonthlyUSD
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return refuse(verb, ns, "monthly_usd must be above zero; a budget cannot be removed or unlimited"), nil
	}
	if v > maxBudgetUSD {
		return refuse(verb, ns, "monthly_usd above $%d is not accepted from an assistant", maxBudgetUSD), nil
	}
	v = math.Round(v*100) / 100
	next := *p
	next.MonthlyUSD = v
	yamlOut, err := r.renderProject(ns, &next)
	if err != nil {
		return Change{}, err
	}
	c := Change{
		Ops:     []FileOp{opFor(st, projectPath(pid), yamlOut)},
		ReadSet: map[string]string{},
		Locks:   []string{lockProject(pid), projectPath(pid)},
		Class:   Inert,
	}
	expect(st, c.ReadSet, projectPath(pid))
	c.Sentence = fmt.Sprintf("Your assistant (%s) lowered the monthly budget of %q to $%s.", ns, pid, formatUSD(v))
	if v > p.MonthlyUSD+1e-9 {
		c.Class = Widening
		clause, _ := spendingTerms(st, v-p.MonthlyUSD, &c.Grant)
		c.Sentence = fmt.Sprintf("Your assistant (%s) wants to raise the monthly budget of %q from $%s to $%s.",
			ns, pid, formatUSD(p.MonthlyUSD), formatUSD(v)) + clause
	}
	return c, nil
}

func (r *Renderer) remove(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbRemove
	ns := st.Namespace
	var in RemoveInput
	if err := decodeStrict(raw, &in); err != nil {
		return refuse(verb, ns, "%v", err), nil
	}
	c := Change{ReadSet: map[string]string{}, Class: Inert}
	var why string
	switch in.Kind {
	case "project":
		why = removeProject(st, agentns.ID(ns, projectSlug(ns, in.ID)), &c)
	case "workflow":
		// Only the form <project>--<workflow> (design section 5, 2026-10-07):
		// a full id cannot be told from it when the project is named for the
		// namespace.
		why = removeWorkflow(st, agentns.ID(ns, in.ID), &c)
		if strings.HasPrefix(why, "there is no workflow") && strings.HasPrefix(in.ID, ns+agentns.Separator) && strings.Count(in.ID, agentns.Separator) >= 2 {
			why = fmt.Sprintf("pass the workflow as %q (<project>--<workflow>, without the namespace %q)", strings.TrimPrefix(in.ID, ns+agentns.Separator), ns+agentns.Separator)
		}
	case "integration":
		var err error
		why, err = r.removeIntegration(st, agentns.ID(ns, projectSlug(ns, in.Project)), in.ID, &c)
		if err != nil {
			return Change{}, err
		}
	case "swarm":
		why = "a project's roles go with the project; use define_swarm to change them"
	case "credential":
		why = "removing a credential is not available in this release"
	default:
		why = "kind must be project, workflow, swarm, integration or credential"
	}
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	c.Sentence = fmt.Sprintf("Your assistant (%s) %s.", ns, c.Sentence)
	return c, nil
}

// deleteOp adds a delete with its read-set expectation and lock.
func deleteOp(st *State, c *Change, path string) {
	c.Ops = append(c.Ops, FileOp{Op: OpDelete, Path: path})
	expect(st, c.ReadSet, path)
	c.Locks = append(c.Locks, path)
}

func removeProject(st *State, pid string, c *Change) string {
	p, ok := st.Projects[pid]
	if !ok {
		return fmt.Sprintf("there is no project %q", pid)
	}
	if p.Home {
		return fmt.Sprintf("%q is your home project and cannot be removed", pid)
	}
	c.Locks = append(c.Locks, lockProject(pid))
	for _, wid := range st.workflowsOf(pid) {
		if _, ok := st.FileHashes[workflowPath(wid)]; ok {
			deleteOp(st, c, workflowPath(wid))
		}
		c.Locks = append(c.Locks, lockWorkflow(wid))
		c.Narrow.RemovedWorkflows = append(c.Narrow.RemovedWorkflows, wid)
	}
	if _, ok := st.FileHashes[swarmPath(p.Swarm.ID)]; ok {
		deleteOp(st, c, swarmPath(p.Swarm.ID))
	}
	deleteOp(st, c, projectPath(pid))
	for _, s := range p.Servers {
		c.Narrow.RemovedIntegrations = append(c.Narrow.RemovedIntegrations, IntegrationRef{Project: pid, Name: s.Name})
	}
	c.Narrow.RemovedProjects = []string{pid}
	c.Sentence = fmt.Sprintf("removed the project %q and its workflows", pid)
	return ""
}

func removeWorkflow(st *State, wid string, c *Change) string {
	if _, ok := st.Workflows[wid]; !ok {
		return fmt.Sprintf("there is no workflow %q", wid)
	}
	owner := ProjectOfWorkflow(wid)
	if p := st.Projects[owner]; p != nil && p.DefaultWorkflowID == wid {
		return fmt.Sprintf("%q is the default workflow of %q and goes with the project", wid, owner)
	}
	c.Locks = append(c.Locks, lockWorkflow(wid), lockProject(owner))
	deleteOp(st, c, workflowPath(wid))
	c.Narrow.RemovedWorkflows = []string{wid}
	c.Sentence = fmt.Sprintf("removed the workflow %q", wid)
	return ""
}

func (r *Renderer) removeIntegration(st *State, pid, name string, c *Change) (string, error) {
	p, ok := st.Projects[pid]
	if !ok {
		return fmt.Sprintf("there is no project %q", pid), nil
	}
	if strings.HasSuffix(name, agentns.WriteSuffix) {
		return fmt.Sprintf("%q is the write side of %q; remove the integration %q", name, agentns.IntegrationOf(name), agentns.IntegrationOf(name)), nil
	}
	next := *p
	next.Servers = nil
	var gone *ServerState
	for i := range p.Servers {
		if agentns.IntegrationOf(p.Servers[i].Name) == name {
			if p.Servers[i].Name == name {
				gone = &p.Servers[i]
			}
			continue // the base entry and its write sibling go together
		}
		next.Servers = append(next.Servers, p.Servers[i])
	}
	var goneAPI *APIState
	next.APIs = nil
	for i := range p.APIs {
		if p.APIs[i].Name == name {
			goneAPI = &p.APIs[i]
			continue
		}
		next.APIs = append(next.APIs, p.APIs[i])
	}
	if gone == nil && goneAPI == nil {
		return fmt.Sprintf("%q has no server or API named %q", pid, name), nil
	}
	if gone == nil {
		gone = &ServerState{Name: goneAPI.Name, AuthRef: goneAPI.AuthRef}
	}
	if gone.AuthRef != "" && !stillReferenced(next.Servers, gone.AuthRef) && !apiReferences(next.APIs, gone.AuthRef) {
		secret := strings.TrimPrefix(gone.AuthRef, "secret://")
		next.Secrets = nil
		for _, s := range p.Secrets {
			if s != secret {
				next.Secrets = append(next.Secrets, s)
			}
		}
	}
	yamlOut, err := r.renderProject(st.Namespace, &next)
	if err != nil {
		return "", err
	}
	c.Ops = []FileOp{opFor(st, projectPath(pid), yamlOut)}
	expect(st, c.ReadSet, projectPath(pid))
	c.Locks = []string{lockProject(pid), projectPath(pid), lockIntegration(pid, name)}
	c.Narrow.RemovedIntegrations = []IntegrationRef{{Project: pid, Name: name}}
	c.Sentence = fmt.Sprintf("disconnected %q from the server %q", pid, name)
	return "", nil
}

func describe(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func stillReferenced(servers []ServerState, ref string) bool {
	for _, s := range servers {
		if s.AuthRef == ref {
			return true
		}
	}
	return false
}

// reachPhrase states a signature's reach in plain words.
func reachPhrase(sig ReachSignature) string {
	if len(sig.Integrations) == 0 {
		return "nothing outside Vornik"
	}
	s := "the servers " + strings.Join(quoteAll(sig.Integrations), ", ")
	if len(sig.Credentials) > 0 {
		var names []string
		for _, c := range sig.Credentials {
			_, ref, _ := strings.Cut(c, "=")
			names = append(names, strings.TrimPrefix(ref, "secret://"))
		}
		s += " using " + strings.Join(names, ", ")
	}
	return s
}

func quoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%q", x)
	}
	return out
}

func quoteShort(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > 120 {
		r = append(r[:117], '.', '.', '.')
	}
	return fmt.Sprintf("%q", string(r))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	r := []rune(line)
	if len(r) > 120 {
		r = r[:120]
	}
	return string(r)
}

func listOrNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// splitWrites returns the advertised tools minus the write tools, or why a
// write tool is refused: one the server does not offer (§7.3).
func splitWrites(advertised, writes []string) ([]string, string) {
	for _, w := range writes {
		if !contains(advertised, w) {
			return nil, fmt.Sprintf("the server does not offer the write tool %q", w)
		}
	}
	var read []string
	for _, t := range advertised {
		if !contains(writes, t) {
			read = append(read, t)
		}
	}
	return read, ""
}

// grantAdvertised fills a new server's read tools from what it advertised
// (minus its write tools), or marks it read-pending when it was not listed.
func grantAdvertised(st *State, url string, writes []string, grant *IntegrationGrant) string {
	advertised, listed := st.Advertised[url]
	if !listed || len(advertised) == 0 {
		grant.ReadPending = true
		return ""
	}
	tools := dedupSorted(advertised)
	for _, t := range tools {
		if !toolNameRe.MatchString(t) {
			return "the server advertises a tool with an unusable name"
		}
	}
	read, why := splitWrites(tools, writes)
	if why != "" {
		return why
	}
	grant.Read = read
	return ""
}

// QueryAPITool is the task agent's REST tool (plan P4.5).
const QueryAPITool = "query_api"

func apiReferences(apis []APIState, ref string) bool {
	for _, a := range apis {
		if a.AuthRef == ref {
			return true
		}
	}
	return false
}

// parseRenderedWorkflow parses a rendered workflow and runs the loader's own
// broker rules on it (proposals' argument schemas included), so a change the
// loader would refuse is refused before it is filed.
func parseRenderedWorkflow(md, id string) (*registry.Workflow, string) {
	wf, err := registry.ParseWorkflowMarkdown([]byte(md), workflowPath(id))
	if err != nil {
		return nil, fmt.Sprintf("the workflow does not load: %v", err)
	}
	if err := wf.Broker.Validate(); err != nil {
		return nil, fmt.Sprintf("the workflow does not load: %v", err)
	}
	return wf, ""
}

// documentSentence is broker design §18.7 F3's clause: the document the
// agent may hand the team, with its real bound and type, and where its text
// can go. "" for a workflow without one.
func documentSentence(sig ReachSignature) string {
	what := documentPhrase(sig.Documents)
	if what == "" {
		return ""
	}
	s := " It takes from your assistant " + what
	if len(sig.Integrations) == 0 {
		return s + "; nothing leaves Vornik."
	}
	its := "its"
	if len(sig.Documents) > 1 {
		its = "their"
	}
	return s + "; the team may use " + its + " text in requests to " + strings.Join(quoteAll(sig.Integrations), ", ") + "."
}

// documentPhrase says the declared documents: "a document of up to 64 KB
// (text/markdown)", or "two documents, of up to 2 KB (text/x-diff) and 4 KB
// (text/markdown)".
func documentPhrase(docs []registry.BrokerDocument) string {
	switch len(docs) {
	case 0:
		return ""
	case 1:
		return "a document of up to " + documentBound(docs[0])
	}
	parts := make([]string, len(docs))
	for i, d := range docs {
		parts[i] = documentBound(d)
	}
	list := strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	return countWord(len(docs)) + " documents, of up to " + list
}

// countWord spells a small count and writes a larger one in digits.
func countWord(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return fmt.Sprintf("%d", n)
}

// documentBound is "64 KB (text/markdown)", or the byte count when the
// bound is not a whole number of KB.
func documentBound(d registry.BrokerDocument) string {
	size := fmt.Sprintf("%d bytes", d.MaxBytes)
	if d.MaxBytes%1024 == 0 {
		size = fmt.Sprintf("%d KB", d.MaxBytes/1024)
	}
	return size + " (" + d.MediaType + ")"
}

func proposeSentence(phrases []string) string {
	if len(phrases) == 0 {
		return ""
	}
	return " It may propose these changes, and you approve each one before it is made: " + strings.Join(phrases, ", ") + "."
}

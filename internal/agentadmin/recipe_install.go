package agentadmin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/registry"
)

// The recipe verbs (agent-administered Vornik design §19.2). list_recipes is
// a read verb the service answers from ListRecipes; install_recipe renders
// one change through Render, composed by §19.11's ordered rule.
const (
	VerbListRecipes   = "list_recipes"
	VerbInstallRecipe = "install_recipe"
)

// InstallRecipeInput is install_recipe. Schedule absent means the recipe's
// schedule_default; JSON null means no schedule; an object replaces it.
type InstallRecipeInput struct {
	Recipe    string                     `json:"recipe"`
	Project   string                     `json:"project"`
	Variables map[string]json.RawMessage `json:"variables,omitempty"`
	Schedule  json.RawMessage            `json:"schedule,omitempty"`
}

// The classes of one needs entry against the project (§19.11).
const (
	EntryNew          = "new"
	EntryReused       = "reused"
	EntryToolsAdded   = "tools_added"
	EntryToolsRemoved = "tools_removed"
	EntryToolsMixed   = "tools_added_and_removed"
)

// entryPlan is one needs entry, substituted and classified.
type entryPlan struct {
	need    RecipeNeed
	url     string
	class   string
	added   []string
	removed []string
	authRef string
	oauth   bool
	// listed: an unauthenticated entry Vornik could list before approval.
	listed bool
	// pending: the existing approval still waits for its tools.
	pending bool
	write   []string
}

// installPlan is everything an install renders from.
type installPlan struct {
	rec       *Recipe
	p         *ProjectState
	wid       string
	vars      map[string]string
	entries   []entryPlan
	schedule  *ScheduleInput
	installed int // the installed version, 0 when not installed
}

func (r *Renderer) installRecipe(st *State, raw json.RawMessage) (Change, error) {
	const verb = VerbInstallRecipe
	ns := st.Namespace
	in, rec, vars, why := r.resolveInstall(ns, raw)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	plan, why := r.planInstall(st, in, rec, vars)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	return r.renderInstall(st, plan)
}

// planInstall resolves the variables, substitutes them by value and
// classifies every entry; any URL difference, credential difference or
// unauthenticated tool check failure refuses the whole install (§19.11 1).
func (r *Renderer) planInstall(st *State, in InstallRecipeInput, rec *Recipe, vars map[string]string) (*installPlan, string) {
	ns := st.Namespace
	pid := agentns.ID(ns, in.Project)
	p, ok := st.Projects[pid]
	if !ok {
		return nil, fmt.Sprintf("there is no project %q; create it first", pid)
	}
	if p.Loaded == nil || p.LoadedSwarm == nil {
		return nil, fmt.Sprintf("the project %q is not loaded yet; try again in a moment", pid)
	}
	wid := WorkflowID(ns, in.Project, rec.Workflow.Slug)
	if len(wid) > maxWorkflowIDLen {
		return nil, fmt.Sprintf("the workflow ID %q is %d characters; use a shorter project slug (the limit is %d)", wid, len(wid), maxWorkflowIDLen)
	}
	plan := &installPlan{rec: rec, p: p, wid: wid}
	if w, ok := st.Workflows[wid]; ok {
		name, version := InstalledRecipe(w.Loaded)
		if name != rec.Name {
			return nil, fmt.Sprintf("%s already has a workflow %q that was not installed from this recipe; remove it first, or install into another project", pid, wid)
		}
		plan.installed = version
	}
	plan.vars = vars
	sched, why := installSchedule(rec, vars, in.Schedule)
	if why != "" {
		return nil, why
	}
	plan.schedule = sched
	for _, n := range rec.Needs {
		e, why := classifyEntry(st, p, n, substitute(n.URL, vars))
		if why != "" {
			return nil, why
		}
		plan.entries = append(plan.entries, e)
	}
	return plan, ""
}

// resolveInstall is the part of an install that needs no state: strict
// decoding, the recipe, the project slug and the variables. The render and
// the pre-approval listing both start from it.
func (r *Renderer) resolveInstall(ns string, raw json.RawMessage) (InstallRecipeInput, *Recipe, map[string]string, string) {
	var in InstallRecipeInput
	if err := decodeStrict(raw, &in); err != nil {
		return in, nil, nil, err.Error()
	}
	in.Project = projectSlug(ns, in.Project)
	rec := r.cat.Get(in.Recipe)
	if rec == nil {
		return in, nil, nil, fmt.Sprintf("there is no recipe %q (list_recipes lists them: %s)", in.Recipe, strings.Join(r.recipeNames(), ", "))
	}
	if err := checkSlug("project", in.Project); err != nil {
		return in, nil, nil, err.Error()
	}
	vars, why := resolveVariables(rec, in.Variables)
	if why != "" {
		return in, nil, nil, why
	}
	return in, rec, vars, ""
}

// resolveVariables validates the agent's values against their types and
// fills defaults; a value is never echoed in a refusal.
func resolveVariables(rec *Recipe, in map[string]json.RawMessage) (map[string]string, string) {
	out := map[string]string{}
	for name, raw := range in {
		v, ok := rec.Variables[name]
		if !ok {
			return nil, fmt.Sprintf("the recipe %s has no variable %q (it has: %s)", rec.Name, name, strings.Join(rec.sortedVariableNames(), ", "))
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Sprintf("the variable %s must be a string", name)
		}
		if why := checkVariableValue(name, v, s); why != "" {
			return nil, why
		}
		out[name] = s
	}
	for _, name := range rec.sortedVariableNames() {
		if _, ok := out[name]; ok {
			continue
		}
		v := rec.Variables[name]
		if v.Default == nil {
			return nil, fmt.Sprintf("the variable %s is required: %s", name, v.Help)
		}
		out[name] = *v.Default
	}
	return out, ""
}

// substitute returns a value with a whole-value variable reference replaced
// by the variable's value: a typed scalar placed into the parsed structure,
// never text spliced into a document (§19.7 F7).
func substitute(value string, vars map[string]string) string {
	if m := varRefRe.FindStringSubmatch(value); m != nil {
		return vars[m[1]]
	}
	return value
}

// installSchedule is the schedule an install renders: the agent's, none
// (JSON null), or the recipe's schedule_default with its variables placed.
func installSchedule(rec *Recipe, vars map[string]string, raw json.RawMessage) (*ScheduleInput, string) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "null":
		return nil, ""
	case trimmed != "":
		var s ScheduleInput
		if err := decodeStrict(raw, &s); err != nil {
			return nil, "schedule: " + err.Error()
		}
		return &s, ""
	case rec.ScheduleDefault == nil:
		return nil, ""
	}
	d := rec.ScheduleDefault
	inputs := map[string]any{}
	for k, v := range d.Inputs {
		if s, ok := v.(string); ok {
			inputs[k] = substitute(s, vars)
			continue
		}
		inputs[k] = v
	}
	rawInputs, err := json.Marshal(inputs)
	if err != nil {
		return nil, "the recipe's schedule inputs are not JSON"
	}
	return &ScheduleInput{Cron: substitute(d.Cron, vars), Timezone: substitute(d.Timezone, vars), Inputs: rawInputs}, ""
}

// classifyEntry decides one entry against the project's integration of the
// same name (§19.8 F1, §19.9 F2, §19.10 F4).
func classifyEntry(st *State, p *ProjectState, n RecipeNeed, url string) (entryPlan, string) {
	e := entryPlan{need: n, url: url}
	if n.Credential != nil {
		switch n.Credential.Kind {
		case CredentialOAuth:
			e.oauth = true
		default:
			e.authRef = "secret://" + st.Namespace + "/" + n.Credential.Name
		}
	}
	for _, a := range p.APIs {
		if a.Name == n.Name {
			return e, fmt.Sprintf("%s already has an API named %q; a recipe needs a server of that name. Remove it first, or install into another project", p.ID, n.Name)
		}
	}
	var existing *ServerState
	for i := range p.Servers {
		if p.Servers[i].Name == n.Name {
			existing = &p.Servers[i]
		}
	}
	read := dedupSorted(n.ReadTools)
	if existing == nil {
		e.class = EntryNew
		if n.Credential == nil {
			if advertised, ok := st.Advertised[url]; ok && len(advertised) > 0 {
				// The unauthenticated tool check, before any approval.
				if missing := minus(read, advertised); len(missing) > 0 {
					return e, fmt.Sprintf("the server at %s does not offer %s; this recipe needs %s", url, strings.Join(missing, ", "), joinWords(read))
				}
				e.listed = true
			}
		}
		return e, ""
	}
	if existing.URL != url {
		return e, fmt.Sprintf("%s already has a server named %q at %s; remove it first, or install into another project", p.ID, n.Name, existing.URL)
	}
	if existing.AuthRef != e.authRef || existing.OAuth != e.oauth {
		return e, fmt.Sprintf("%s already has a server named %q at %s with another credential; remove it first, or install into another project", p.ID, n.Name, existing.URL)
	}
	a, ok := st.Approvals[p.ID][n.Name]
	if !ok || !a.Live() {
		return e, fmt.Sprintf("the server %q of %s is not approved; remove it first, or install into another project", n.Name, p.ID)
	}
	e.write = a.Write
	if a.ReadPending {
		e.class, e.pending = EntryReused, true
		return e, ""
	}
	e.added, e.removed = minus(read, a.Read), minus(dedupSorted(a.Read), read)
	switch {
	case len(e.added) > 0 && len(e.removed) > 0:
		e.class = EntryToolsMixed
	case len(e.added) > 0:
		e.class = EntryToolsAdded
	case len(e.removed) > 0:
		e.class = EntryToolsRemoved
	default:
		e.class = EntryReused
	}
	return e, ""
}

// minus is a − b, sorted.
func minus(a, b []string) []string {
	var out []string
	for _, x := range dedupSorted(a) {
		if !contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// renderedInstall is an install's three files and the loaded workflow.
type renderedInstall struct {
	projectYAML, swarmMD, workflowMD, swarmID string
	wf                                        *registry.Workflow
	sig                                       ReachSignature
}

// renderInstall renders the one change and composes its class (§19.11 2-4).
func (r *Renderer) renderInstall(st *State, plan *installPlan) (Change, error) {
	const verb = VerbInstallRecipe
	ns, p := st.Namespace, plan.p
	next := nextProject(plan)
	roles, dropped, why := nextRoles(plan.rec, p, plan.installed > 0)
	if why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	if why := toolsStillHeld(plan, roles); why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	if why := droppedRoleInUse(st, p.ID, plan.wid, dropped); why != "" {
		return refuse(verb, ns, "%s", why), nil
	}
	next.Swarm = SwarmState{ID: p.Swarm.ID, Roles: roles}
	if next.Swarm.ID == "" {
		next.Swarm.ID = p.ID
	}
	out, why, err := r.renderInstallFiles(st, plan, next)
	if err != nil || why != "" {
		if why != "" {
			return refuse(verb, ns, "%s", why), nil
		}
		return Change{}, err
	}
	return composeInstall(st, plan, out), nil
}

// renderInstallFiles renders the project, swarm and workflow, re-parses
// them with the registry's parsers and computes the workflow's reach from
// those, as the executor will (§7.6).
func (r *Renderer) renderInstallFiles(st *State, plan *installPlan, next *ProjectState) (*renderedInstall, string, error) {
	ns, rec := st.Namespace, plan.rec
	out := &renderedInstall{swarmID: next.Swarm.ID}
	var err error
	if out.projectYAML, err = r.renderProject(ns, next); err != nil {
		return nil, "", err
	}
	if out.swarmMD, err = r.renderSwarm(st, next); err != nil {
		return nil, "", err
	}
	loadedSwarm, err := registry.ParseSwarmMarkdown([]byte(out.swarmMD), swarmPath(next.Swarm.ID))
	if err != nil {
		return nil, "", fmt.Errorf("rendered swarm does not parse: %w", err)
	}
	var loadedProject registry.Project
	if err := yaml.Unmarshal([]byte(out.projectYAML), &loadedProject); err != nil {
		return nil, "", fmt.Errorf("rendered project does not parse: %w", err)
	}
	schedule, why := renderSchedule(plan.schedule)
	if why != "" {
		return nil, why, nil
	}
	data := workflowData{
		Namespace: ns, ID: plan.wid, DisplayName: rec.Workflow.Slug, Description: rec.Workflow.Purpose,
		Entrypoint: rec.Workflow.Steps[0].Name, InputSchema: rec.inputSchema, EgressSchema: rec.egressJSON,
		MaxBytes: egressDocBytes, Steps: recipeSteps(rec), Schedule: schedule, Version: recipeVersionTag(rec.Name, rec.Version),
	}
	if out.workflowMD, err = r.exec(tmplWorkflow, data); err != nil {
		return nil, "", err
	}
	if out.wf, why = parseRenderedWorkflow(out.workflowMD, plan.wid); why != "" {
		return nil, why, nil
	}
	if out.sig, err = SignatureOf(&loadedProject, loadedSwarm, out.wf); err != nil {
		return nil, "", err
	}
	return out, "", nil
}

// composeInstall applies §19.11's ordered rule to a rendered install: one
// approval when an entry is new or adds tools, or the workflow's reach
// changed (removals ride with it); else a narrowing applied at once; else
// inert, with nothing to file when no file differs.
func composeInstall(st *State, plan *installPlan, out *renderedInstall) Change {
	ns, rec, p, wid := st.Namespace, plan.rec, plan.p, plan.wid
	hash := out.sig.Hash()
	prev := st.Workflows[wid]
	reachChanged := prev == nil || prev.ApprovedReach != hash
	c := Change{ReadSet: map[string]string{}, Class: Inert, reach: &out.sig, recipe: rec}
	c.Locks = []string{lockProject(p.ID), lockWorkflow(wid), projectPath(p.ID), swarmPath(out.swarmID), workflowPath(wid)}
	for _, f := range []struct{ path, content string }{
		{projectPath(p.ID), out.projectYAML}, {swarmPath(out.swarmID), out.swarmMD}, {workflowPath(wid), out.workflowMD},
	} {
		expect(st, c.ReadSet, f.path)
		if h, ok := st.FileHashes[f.path]; ok && h == HashContent([]byte(f.content)) {
			continue // identical: nothing to write
		}
		c.Ops = append(c.Ops, opFor(st, f.path, f.content))
	}
	widening := reachChanged
	for _, e := range plan.entries {
		c.Locks = append(c.Locks, lockIntegration(p.ID, e.need.Name))
		switch e.class {
		case EntryNew, EntryToolsAdded, EntryToolsMixed:
			widening = true
			c.Grant.Integrations = append(c.Grant.Integrations, entryGrant(p.ID, e))
		}
		if len(e.removed) > 0 {
			c.Narrow.RemovedTools = append(c.Narrow.RemovedTools, ToolRemoval{Project: p.ID, Name: e.need.Name, Tools: e.removed, Kind: NarrowingRecipeTools})
		}
	}
	switch {
	case widening:
		c.Class = Widening
		if reachChanged {
			c.Grant.Workflows = map[string]string{wid: hash}
		}
		c.Credentials = credentialFollowUps(rec, p.ID)
		c.Sentence = installSentence(ns, rec, p.ID, plan, wid, out.wf, prev)
	case len(c.Narrow.RemovedTools) > 0:
		c.Sentence = fmt.Sprintf("Your assistant (%s) updated %q in %s to version %d; %s.", ns, rec.Title, p.ID, rec.Version, removalPhrase(plan))
	case len(c.Ops) == 0:
		c.Sentence = fmt.Sprintf("%q is already installed in %s at version %d with these settings; nothing changed.", rec.Title, p.ID, rec.Version)
		c.Locks = nil
	default:
		c.Sentence = fmt.Sprintf("Your assistant (%s) updated %q in %s to version %d; what it can reach and what it returns are unchanged.", ns, rec.Title, p.ID, rec.Version)
	}
	return c
}

// entryGrant is what approving an entry records: its read set becomes
// exactly the recipe's read_tools. A server Vornik could not list without
// its credential stays read-pending, and its tools are approved after the
// credential is stored (§19.8 F6, the tools-approval check).
func entryGrant(pid string, e entryPlan) IntegrationGrant {
	g := IntegrationGrant{Project: pid, Name: e.need.Name, Kind: "mcp", URL: e.url, Write: e.write}
	if e.class == EntryNew && !e.listed {
		g.ReadPending = true
		return g
	}
	g.Read = dedupSorted(e.need.ReadTools)
	return g
}

// nextProject is the project with the recipe's servers and secrets.
func nextProject(plan *installPlan) *ProjectState {
	p := plan.p
	next := *p
	next.Servers = append([]ServerState(nil), p.Servers...)
	next.Secrets = append([]string(nil), p.Secrets...)
	for _, e := range plan.entries {
		tools := dedupSorted(e.need.ReadTools)
		switch e.class {
		case EntryNew:
			next.Servers = append(next.Servers, ServerState{Name: e.need.Name, URL: e.url, AuthRef: e.authRef, OAuth: e.oauth, Tools: tools})
		case EntryToolsAdded, EntryToolsRemoved, EntryToolsMixed:
			for i := range next.Servers {
				if next.Servers[i].Name == e.need.Name {
					next.Servers[i].Tools = tools
				}
			}
		}
		if e.authRef != "" {
			if ref := strings.TrimPrefix(e.authRef, "secret://"); !contains(next.Secrets, ref) {
				next.Secrets = append(next.Secrets, ref)
			}
		}
	}
	return &next
}

// nextRoles is the swarm's roles with the recipe's, each named
// <recipe>-<role> (§19.7 F4). An installed recipe's own roles are replaced;
// any other role of the same full name is refused, so nothing is applied
// (§19.8 test 5). It also returns the names of roles the install drops.
func nextRoles(rec *Recipe, p *ProjectState, installed bool) ([]RoleSpec, []string, string) {
	mine := map[string]bool{}
	for _, role := range rec.Team {
		mine[rec.RoleName(role.Name)] = true
	}
	var out []RoleSpec
	var dropped []string
	for _, role := range p.Swarm.Roles {
		ours := installed && strings.HasPrefix(role.Name, rec.Name+"-")
		if ours {
			if !mine[role.Name] {
				dropped = append(dropped, role.Name)
			}
			continue
		}
		if mine[role.Name] {
			return nil, nil, fmt.Sprintf("%s already has a role named %q; rename or remove it first, or install into another project", p.ID, role.Name)
		}
		if role.Instructions == "" {
			// A role with no text at all; since §18.11 the read-back carries
			// each role's instructions, so this is no longer the common case.
			role.Instructions = role.Description
		}
		out = append(out, role)
	}
	for _, role := range rec.Team {
		tools := dedupSorted(append(append([]string(nil), role.Tools...), "file_write"))
		out = append(out, RoleSpec{Name: rec.RoleName(role.Name), Description: firstLine(role.Instructions),
			Instructions: strings.TrimSpace(role.Instructions), Tools: tools})
	}
	if len(out) > 0 && !hasRole(out, defaultRole().Name) {
		out = append(out, defaultRole())
	}
	return out, dropped, ""
}

func hasRole(roles []RoleSpec, name string) bool {
	for _, r := range roles {
		if r.Name == name {
			return true
		}
	}
	return false
}

// toolsStillHeld refuses removing a read tool another role still holds: its
// workflows would be refused at execution.
func toolsStillHeld(plan *installPlan, roles []RoleSpec) string {
	for _, e := range plan.entries {
		for _, t := range e.removed {
			full := "mcp__" + e.need.Name + "__" + t
			for _, role := range roles {
				if strings.HasPrefix(role.Name, plan.rec.Name+"-") {
					continue
				}
				if contains(role.Tools, full) {
					return fmt.Sprintf("this version no longer uses %s of %q, but the role %q still holds it; change that role first", t, e.need.Name, role.Name)
				}
			}
		}
	}
	return ""
}

// droppedRoleInUse refuses dropping a role another workflow still runs.
func droppedRoleInUse(st *State, project, own string, dropped []string) string {
	for _, wid := range st.workflowsOf(project) {
		w := st.Workflows[wid]
		if wid == own || w.Loaded == nil {
			continue
		}
		for name, step := range w.Loaded.Steps {
			if contains(dropped, step.Role) {
				return fmt.Sprintf("workflow %s step %s uses the role %q, which this version removes", wid, name, step.Role)
			}
		}
	}
	return ""
}

// recipeSteps links the recipe's steps in order, the last to done, with the
// hand-off text of design §18.10.
func recipeSteps(rec *Recipe) []stepData {
	steps := rec.Workflow.Steps
	out := make([]stepData, 0, len(steps))
	for i, s := range steps {
		next := "done"
		if i+1 < len(steps) {
			next = steps[i+1].Name
		}
		closing := closingOneStep
		if len(steps) > 1 {
			closing = closingMultiStep
		}
		last := i+1 == len(steps)
		requireOutputGlob := ""
		if !last {
			requireOutputGlob = "artifacts/out/" + s.Name + ".md"
		}
		out = append(out, stepData{Name: s.Name, Role: rec.RoleName(s.Role), Instructions: strings.TrimSpace(s.Instructions), Next: next,
			RequireOutputGlob: requireOutputGlob, First: i == 0, Last: last, Closing: closing})
	}
	return out
}

// credentialFollowUps is one request per distinct credential (§19.9 F1,
// §19.11 F5): entries naming the same credential share it.
func credentialFollowUps(rec *Recipe, pid string) []CredentialFollowUp {
	reads := map[string][]string{}
	kinds := map[string]string{}
	var names []string
	for _, n := range rec.Needs {
		if n.Credential == nil {
			continue
		}
		if _, ok := kinds[n.Credential.Name]; !ok {
			names = append(names, n.Credential.Name)
		}
		kinds[n.Credential.Name] = n.Credential.Kind
		reads[n.Credential.Name] = append(reads[n.Credential.Name], n.Reads)
	}
	sort.Strings(names)
	out := make([]CredentialFollowUp, 0, len(names))
	for _, name := range names {
		out = append(out, CredentialFollowUp{Project: pid, Name: name, Kind: kinds[name],
			Purpose: fmt.Sprintf("Read %s for the %s recipe", joinWords(reads[name]), rec.Title)})
	}
	return out
}

// installSentence is the approval sentence: the catalogue's fixed title and
// plain line, and only daemon-computed reach slots (the project, each
// server's name, URL and tools, the schedule): no variable or agent text
// reaches it otherwise (§19.7 F3, §19.8).
func installSentence(ns string, rec *Recipe, pid string, plan *installPlan, wid string, wf *registry.Workflow, prev *WorkflowState) string {
	var b strings.Builder
	if plan.installed > 0 {
		fmt.Fprintf(&b, "Your assistant (%s) wants to upgrade %q in %s from version %d to version %d", ns, rec.Title, pid, plan.installed, rec.Version)
	} else {
		fmt.Fprintf(&b, "Your assistant (%s) wants to install %q (version %d) in %s", ns, rec.Title, rec.Version, pid)
	}
	var reads []string
	for _, e := range plan.entries {
		s := fmt.Sprintf("%s through the server %q at %s", e.need.Reads, e.need.Name, e.url)
		switch e.class {
		case EntryNew:
			s += fmt.Sprintf(" (it works with a server that offers %s)", joinWords(dedupSorted(e.need.ReadTools)))
		case EntryToolsAdded:
			s += fmt.Sprintf(", now also with %s", joinWords(e.added))
		case EntryToolsMixed:
			s += fmt.Sprintf(", now also with %s and no longer with %s", joinWords(e.added), joinWords(e.removed))
		case EntryToolsRemoved:
			s += fmt.Sprintf(", no longer with %s", joinWords(e.removed))
		}
		reads = append(reads, s)
	}
	fmt.Fprintf(&b, ": it reads %s, and returns %s.", joinWords(reads), rec.Returns)
	b.WriteString(scheduleSentence(wid, wf.Broker.Schedule, prev))
	var creds []string
	for _, f := range credentialFollowUps(rec, pid) {
		creds = append(creds, f.Name)
	}
	if len(creds) > 0 {
		fmt.Fprintf(&b, " It uses the credential %s; you enter any that is not set yet on your phone after you approve.", joinWords(creds))
	}
	return b.String()
}

// removalPhrase names the tools a narrowing drops.
func removalPhrase(plan *installPlan) string {
	var parts []string
	for _, e := range plan.entries {
		if len(e.removed) > 0 {
			parts = append(parts, fmt.Sprintf("it no longer uses %s of %q", joinWords(e.removed), e.need.Name))
		}
	}
	return strings.Join(parts, "; ")
}

func (r *Renderer) recipeNames() []string {
	var out []string
	for _, rec := range r.cat.Recipes {
		out = append(out, rec.Name)
	}
	return out
}

// RecipeListTargets returns the URLs of an install's entries that declare no
// credential: the only servers Vornik may list before an approval (the
// unauthenticated tool check, §19.8 F6). Nil when the input does not
// resolve; the render then refuses it with the reason.
func (r *Renderer) RecipeListTargets(ns string, input json.RawMessage) []string {
	// One resolution with the install (review 20261003-e379 F6): an input
	// the install would refuse is never listed.
	_, rec, vars, why := r.resolveInstall(ns, input)
	if why != "" {
		return nil
	}
	var out []string
	for _, n := range rec.Needs {
		if n.Credential == nil {
			out = append(out, substitute(n.URL, vars))
		}
	}
	return out
}

// RecipeView is one recipe as list_recipes shows it (§19.2, §19.8 F2).
type RecipeView struct {
	Name            string               `json:"name"`
	Version         int                  `json:"version"`
	Title           string               `json:"title"`
	Summary         string               `json:"summary"`
	EnvelopeVersion int                  `json:"envelope_version"`
	Variables       []RecipeVariableView `json:"variables"`
	Needs           []RecipeNeedView     `json:"needs"`
	LinkHosts       []string             `json:"link_hosts"`
	Returns         RecipeReturns        `json:"returns"`
	ScheduleDefault *RecipeSchedule      `json:"schedule_default,omitempty"`
	// Sentence is the approval sentence of a fresh install, with the
	// daemon-filled slots shown as <placeholders>.
	Sentence  string          `json:"sentence"`
	Installed []RecipeInstall `json:"installed"`
}

// RecipeVariableView is one variable.
type RecipeVariableView struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Help     string   `json:"help,omitempty"`
	Values   []string `json:"values,omitempty"`
	Default  *string  `json:"default,omitempty"`
	Required bool     `json:"required"`
}

// RecipeNeedView is one needs entry.
type RecipeNeedView struct {
	Name       string            `json:"name"`
	Reads      string            `json:"reads"`
	URL        string            `json:"url"`
	ReadTools  []string          `json:"read_tools"`
	Credential *RecipeCredential `json:"credential,omitempty"`
	WorksWith  string            `json:"works_with"`
}

// RecipeReturns is what a recipe returns: its plain line, the egress fields
// in words, and the envelope fields every recipe carries.
type RecipeReturns struct {
	Plain    string   `json:"plain"`
	Fields   []string `json:"fields"`
	Envelope []string `json:"envelope"`
}

// RecipeInstall is one installation in the namespace.
type RecipeInstall struct {
	Project  string `json:"project"`
	Workflow string `json:"workflow"`
	Version  int    `json:"version"`
}

// ListRecipes is list_recipes for the namespace in st.
func (r *Renderer) ListRecipes(st *State) []RecipeView {
	out := []RecipeView{}
	for _, rec := range r.cat.Recipes {
		v := RecipeView{Name: rec.Name, Version: rec.Version, Title: rec.Title, Summary: rec.Summary,
			EnvelopeVersion: rec.EnvelopeVersion, LinkHosts: append([]string{}, rec.LinkHosts...),
			Variables: []RecipeVariableView{}, Needs: []RecipeNeedView{}, Installed: []RecipeInstall{},
			ScheduleDefault: rec.ScheduleDefault,
			Returns: RecipeReturns{Plain: rec.Returns, Fields: rec.egressLines,
				Envelope: []string{"status", "errors", "empty", "as_of", "items[].source", "items[].id", "items[].link"}}}
		if len(rec.LinkHosts) == 0 {
			v.Returns.Envelope = v.Returns.Envelope[:len(v.Returns.Envelope)-1]
		}
		for _, name := range rec.sortedVariableNames() {
			vr := rec.Variables[name]
			v.Variables = append(v.Variables, RecipeVariableView{Name: name, Type: vr.Type, Help: vr.Help, Values: vr.Values, Default: vr.Default, Required: vr.Default == nil})
		}
		placeholders := map[string]string{}
		for name := range rec.Variables {
			placeholders[name] = "<" + name + ">"
		}
		plan := &installPlan{rec: rec}
		for _, n := range rec.Needs {
			v.Needs = append(v.Needs, RecipeNeedView{Name: n.Name, Reads: n.Reads, URL: n.URL, ReadTools: dedupSorted(n.ReadTools),
				Credential: n.Credential, WorksWith: "works with a server that offers " + joinWords(dedupSorted(n.ReadTools))})
			plan.entries = append(plan.entries, entryPlan{need: n, url: substitute(n.URL, placeholders), class: EntryNew})
		}
		v.Sentence = installSentenceTemplate(st.Namespace, rec, plan)
		for _, id := range sortedWorkflowKeys(st) {
			w := st.Workflows[id]
			if name, version := InstalledRecipe(w.Loaded); name == rec.Name {
				v.Installed = append(v.Installed, RecipeInstall{Project: w.Project, Workflow: id, Version: version})
			}
		}
		out = append(out, v)
	}
	return out
}

// installSentenceTemplate is a fresh install's sentence with placeholders.
func installSentenceTemplate(ns string, rec *Recipe, plan *installPlan) string {
	wf := &registry.Workflow{Broker: &registry.WorkflowBroker{}}
	if d := rec.ScheduleDefault; d != nil {
		placeholders := map[string]string{}
		for name := range rec.Variables {
			placeholders[name] = "<" + name + ">"
		}
		inputs := map[string]any{}
		for k, v := range d.Inputs {
			if s, ok := v.(string); ok {
				v = substitute(s, placeholders)
			}
			inputs[k] = v
		}
		wf.Broker.Schedule = &registry.BrokerSchedule{Cron: substitute(d.Cron, placeholders), Timezone: substitute(d.Timezone, placeholders), Inputs: inputs}
	}
	return installSentence(ns, rec, "<project>", plan, "<workflow>", wf, nil)
}

func sortedWorkflowKeys(st *State) []string {
	out := make([]string, 0, len(st.Workflows))
	for id := range st.Workflows {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

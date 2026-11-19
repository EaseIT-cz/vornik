package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/safepath"
)

// "Use in a project" on the control-plane hub's MCP tab
// (2026-07-08-control-plane-hub-design.md, amendment 2026-11-15, GitHub
// #627): pick a project, tick which roles of its swarm may call which of the
// server's tools, and file one proposal that subscribes the project and
// edits the roles' allowlists. The page files a proposal and writes nothing;
// apply is the engine's.

// Role modes on the grid (U1).
const (
	mcpUseGrid    = "grid"    // an explicit allowlist: one box per tool
	mcpUseFree    = "free"    // no allowlist: every subscribed tool already, no boxes
	mcpUsePattern = "pattern" // a pattern covers the server: every tool already, no boxes
)

// mcpUseRole is one role of the chosen project's swarm.
type mcpUseRole struct {
	Name    string
	Mode    string
	Granted map[string]bool // the server's short tool name → granted now
}

// Has reports whether the role may call tool now (the template's check; the
// console's FuncMap replaces the builtin index).
func (r mcpUseRole) Has(tool string) bool { return r.Granted[tool] }

// mcpUseGrantRow is one line of the read-only current grants.
type mcpUseGrantRow struct{ Project, Role, Access string }

// MCPUseData backs admin_control_plane_mcp_use.html.
type MCPUseData struct {
	Title       string
	CurrentPage string
	Server      string
	Tools       []MCPRegistryTool
	Unlisted    bool
	Projects    []string
	Project     string
	Roles       []mcpUseRole
	SharedWith  []string
	Grants      []mcpUseGrantRow
	Error       string

	// The project page's mode (#780): the project is fixed, a server is
	// picked, the button applies, and pending applies are listed.
	ProjectMode bool
	Servers     []string
	Pending     []mcpToolsPending
	Flash       string
	// OAuth is the chosen server's sign-in row (#765), nil when the server
	// is not OAuth-mode or the caller lacks mcp_oauth_connect.
	OAuth *mcpToolsOAuth
}

// mcpQualified is a tool's name as role allowlists hold it.
func mcpQualified(server, tool string) string { return "mcp__" + server + "__" + tool }

// mcpUseRoleOf classifies role r for server's tools.
func mcpUseRoleOf(r registry.SwarmRole, server string, tools []MCPRegistryTool) mcpUseRole {
	out := mcpUseRole{Name: r.Name, Mode: mcpUseGrid, Granted: map[string]bool{}}
	list := r.Permissions.AllowedTools
	if len(list) == 0 {
		out.Mode = mcpUseFree
		return out
	}
	exact := map[string]bool{}
	for _, e := range list {
		exact[e] = true
	}
	for _, e := range list {
		if !strings.Contains(e, "*") {
			continue
		}
		for _, t := range tools {
			if agenttools.AllowlistAdmits([]string{e}, mcpQualified(server, t.Name)) {
				out.Mode = mcpUsePattern
				return out
			}
		}
	}
	for _, t := range tools {
		out.Granted[t.Name] = exact[mcpQualified(server, t.Name)]
	}
	return out
}

// mcpUseAccess is a role's access line for the current grants table.
func mcpUseAccess(role mcpUseRole, tools []MCPRegistryTool) string {
	switch role.Mode {
	case mcpUseFree:
		return "every tool (this role declares no allowlist)"
	case mcpUsePattern:
		return "every tool, by pattern"
	}
	var names []string
	for _, t := range tools {
		if role.Granted[t.Name] {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func (s *Server) mcpUseServer(ctx context.Context, name string) ([]MCPRegistryTool, bool) {
	if s.mcpRegistrySource == nil {
		return nil, false
	}
	servers, err := s.mcpRegistrySource.Servers(ctx)
	if err != nil {
		return nil, false
	}
	for _, sv := range servers {
		if sv.Name == name {
			return sv.Tools, true
		}
	}
	return nil, false
}

// mcpUseBuild fills the page's data for server and, when set, project.
func (s *Server) mcpUseBuild(ctx context.Context, server, project string) MCPUseData {
	data := MCPUseData{Title: "Use " + server + " in a project", CurrentPage: "admin-control-plane", Server: server, Project: project}
	tools, known := s.mcpUseServer(ctx, server)
	if !known {
		data.Error = "No MCP server named " + server + " in the registry."
		return data
	}
	data.Tools = tools
	data.Unlisted = len(tools) == 0
	if s.projectReg == nil {
		data.Error = "Project registry not configured."
		return data
	}
	projects := s.projectReg.ListProjects()
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	for _, p := range projects {
		data.Projects = append(data.Projects, p.ID)
		if !projectSubscribes(p, server) {
			continue
		}
		if sw := s.projectReg.GetSwarm(p.SwarmID); sw != nil {
			for _, r := range sw.Roles {
				data.Grants = append(data.Grants, mcpUseGrantRow{Project: p.ID, Role: r.Name, Access: mcpUseAccess(mcpUseRoleOf(r, server, tools), tools)})
			}
		}
	}
	if project == "" {
		return data
	}
	p := s.projectReg.GetProject(project)
	if p == nil {
		data.Error = "No project named " + project + "."
		return data
	}
	sw := s.projectReg.GetSwarm(p.SwarmID)
	if sw == nil {
		data.Error = "Project " + project + " has no swarm."
		return data
	}
	for _, r := range sw.Roles {
		data.Roles = append(data.Roles, mcpUseRoleOf(r, server, tools))
	}
	data.SharedWith = s.projectsSharingSwarm(p)
	return data
}

func projectSubscribes(p *registry.Project, server string) bool {
	for _, sv := range p.MCP.Servers {
		if sv.Name == server {
			return true
		}
	}
	return false
}

// projectsSharingSwarm lists the other projects whose swarm is p's.
func (s *Server) projectsSharingSwarm(p *registry.Project) []string {
	var out []string
	for _, o := range s.projectReg.ListProjects() {
		if o.ID != p.ID && o.SwarmID == p.SwarmID {
			out = append(out, o.ID)
		}
	}
	sort.Strings(out)
	return out
}

// AdminControlPlaneMCPUse serves GET and POST
// /ui/admin/control-plane/mcp/use (amendment 2026-11-15, U1 and U2). It is
// mounted on the admin router, which admits only an admin.
func (s *Server) AdminControlPlaneMCPUse(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		server := strings.TrimSpace(r.URL.Query().Get("server"))
		if !mcpServerNameRe.MatchString(server) {
			http.Redirect(w, r, "/ui/admin/control-plane?section=mcp&done=mcp-bad-name", http.StatusSeeOther)
			return
		}
		s.render(w, "admin_control_plane_mcp_use.html", s.mcpUseBuild(r.Context(), server, strings.TrimSpace(r.URL.Query().Get("project"))))
	case http.MethodPost:
		s.mcpUsePropose(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// errMCPUseRefused is a post the page never offers (a forged grid).
var errMCPUseRefused = fmt.Errorf("refused")

// mcpUseTicks validates the posted "<role>|<tool>" boxes against the grid:
// only grid roles, only the registry's tools for this server.
func mcpUseTicks(values []string, data MCPUseData) (map[string]map[string]bool, error) {
	grid := map[string]bool{}
	for _, r := range data.Roles {
		if r.Mode == mcpUseGrid {
			grid[r.Name] = true
		}
	}
	listed := map[string]bool{}
	for _, t := range data.Tools {
		listed[t.Name] = true
	}
	want := map[string]map[string]bool{}
	for role := range grid {
		want[role] = map[string]bool{}
	}
	for _, v := range values {
		role, tool, ok := strings.Cut(v, "|")
		if !ok || !grid[role] || !listed[tool] {
			return nil, errMCPUseRefused
		}
		want[role][tool] = true
	}
	return want, nil
}

func (s *Server) mcpUsePropose(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	_ = r.ParseForm()
	server := strings.TrimSpace(r.PostFormValue("server"))
	project := strings.TrimSpace(r.PostFormValue("project"))
	filed := s.mcpUseFileFromForm(r, server, project, "", false)
	switch filed.token {
	case "":
		http.Redirect(w, r, "/ui/admin/control-plane?section=proposals&done=mcp-use-proposed", http.StatusSeeOther)
	case mcpUseUnchanged:
		http.Redirect(w, r, "/ui/admin/control-plane/mcp/use?server="+url.QueryEscape(server)+"&project="+url.QueryEscape(project)+"&done="+mcpUseUnchanged, http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/ui/admin/control-plane?section=mcp&done="+filed.token, http.StatusSeeOther)
	}
}

// Outcome tokens of mcpUseFileFromForm.
const (
	mcpUseRefused   = "mcp-use-refused"
	mcpUseUnchanged = "mcp-use-unchanged"
	mcpUseAckNeeded = "ack-required"
)

// mcpUseFiled is what mcpUseFileFromForm did: a filed proposal's id and the
// plan's per-role lines, or the token saying why nothing was filed.
type mcpUseFiled struct {
	id, token string
	data      MCPUseData
	granted   []string
}

// mcpUseFileFromForm is the one validate, plan and file step of both pages
// (#627's hub page, #780's project page): the posted "grant" boxes are
// checked against the grid the page offers, the two files planned, and one
// config proposal filed with source in its Evidence. A shared swarm needs
// ackShared, refused before anything is filed (amendment 2026-11-15 #780,
// O3), when requireAck is set.
func (s *Server) mcpUseFileFromForm(r *http.Request, server, project, source string, requireAck bool) mcpUseFiled {
	if !mcpServerNameRe.MatchString(server) || project == "" || s.proposalStore == nil || s.cpConfigPath == "" {
		return mcpUseFiled{token: mcpUseRefused}
	}
	data := s.mcpUseBuild(r.Context(), server, project)
	if data.Error != "" || data.Unlisted {
		return mcpUseFiled{token: mcpUseRefused}
	}
	want, err := mcpUseTicks(r.PostForm["grant"], data)
	if err != nil {
		return mcpUseFiled{token: mcpUseRefused}
	}
	plan, err := s.mcpUsePlan(server, project, data, want)
	if err != nil {
		s.logger.Warn().Err(err).Str("server", server).Str("project", project).Msg("control plane: MCP use could not be planned")
		return mcpUseFiled{token: mcpUseRefused}
	}
	if len(plan.ops) == 0 {
		return mcpUseFiled{token: mcpUseUnchanged, data: data}
	}
	ack := r.PostFormValue("ackShared") == "on"
	if requireAck && len(data.SharedWith) > 0 && !ack {
		return mcpUseFiled{token: mcpUseAckNeeded, data: data}
	}
	id, err := s.mcpUseFile(r, server, project, data, plan, source, ack)
	if err != nil {
		s.logger.Warn().Err(err).Msg("control plane: MCP use proposal not filed")
		return mcpUseFiled{token: mcpUseRefused}
	}
	return mcpUseFiled{id: id, data: data, granted: plan.granted}
}

// mcpUseOp is one file of the proposal, with what the read set needs.
type mcpUseOp struct {
	rel, before, after string
}

type mcpUsePlanned struct {
	ops     []mcpUseOp
	granted []string // "role: tools" lines for the rationale
}

// mcpUsePlan computes the two files' new contents (U2). An op is planned
// only for a file that changes.
func (s *Server) mcpUsePlan(server, project string, data MCPUseData, want map[string]map[string]bool) (mcpUsePlanned, error) {
	var out mcpUsePlanned
	configDir := s.configDir()
	p := s.projectReg.GetProject(project)
	if configDir == "" || p == nil {
		return out, fmt.Errorf("no config tree or project")
	}
	engineDir := filepath.Dir(s.cpConfigPath)
	rel := func(abs string) (string, error) {
		r, err := filepath.Rel(engineDir, abs)
		return filepath.ToSlash(r), err
	}
	anyTick := false
	var needTools []string
	for _, t := range data.Tools {
		for _, role := range data.Roles {
			if want[role.Name][t.Name] {
				anyTick = true
				needTools = append(needTools, t.Name)
				break
			}
		}
	}

	swarmPath, err := safepath.JoinUnder(configDir, "swarms", p.SwarmID+".md")
	if err != nil {
		return out, err
	}
	swarmBefore, err := os.ReadFile(swarmPath) //nolint:gosec // confined under the registry's config tree
	if err != nil {
		return out, err
	}
	swarmAfter, granted, err := editSwarmMCPGrants(swarmBefore, filepath.Base(swarmPath), server, data, want)
	if err != nil {
		return out, err
	}
	out.granted = granted
	if anyTick {
		projectPath, err := safepath.JoinUnder(configDir, "projects", project+".yaml")
		if err != nil {
			return out, err
		}
		projectBefore, err := os.ReadFile(projectPath) //nolint:gosec // confined under the registry's config tree
		if err != nil {
			return out, err
		}
		projectAfter, err := editProjectMCPSubscription(projectBefore, server, needTools)
		if err != nil {
			return out, err
		}
		if !bytes.Equal(projectBefore, projectAfter) {
			r, err := rel(projectPath)
			if err != nil {
				return out, err
			}
			out.ops = append(out.ops, mcpUseOp{rel: r, before: string(projectBefore), after: string(projectAfter)})
		}
	}
	if !bytes.Equal(swarmBefore, swarmAfter) {
		r, err := rel(swarmPath)
		if err != nil {
			return out, err
		}
		out.ops = append(out.ops, mcpUseOp{rel: r, before: string(swarmBefore), after: string(swarmAfter)})
	}
	return out, nil
}

// mcpUseFile files the planned ops as one config proposal (U2) and returns
// its id. source and ackShared go into the Evidence (#780 O3, O4).
func (s *Server) mcpUseFile(r *http.Request, server, project string, data MCPUseData, plan mcpUsePlanned, source string, ackShared bool) (string, error) {
	type op struct {
		Op      string `json:"op"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	ops := make([]op, 0, len(plan.ops))
	readSet := map[string]string{}
	var diff strings.Builder
	for _, o := range plan.ops {
		ops = append(ops, op{Op: "replace", Path: o.rel, Content: o.after})
		readSet[o.rel] = sha256Hex([]byte(o.before))
		fmt.Fprintf(&diff, "--- %s\n+++ %s\n%s\n", o.rel, o.rel, unifiedish(o.before, o.after))
	}
	rawOps, err := json.Marshal(ops)
	if err != nil {
		return "", err
	}
	actor := keysActor(r)
	ev := map[string]any{"read_set": readSet, "actor": actor}
	if source != "" {
		ev["source"], ev["ack_shared"] = source, ackShared
	}
	evidence, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	scope := persistence.ProposalScopeProject
	shared := ""
	if len(data.SharedWith) > 0 {
		scope = persistence.ProposalScopeSwarm
		shared = " The swarm also serves " + strings.Join(data.SharedWith, ", ") + ": a role grant applies to them too where they subscribe " + server + "."
	}
	var roles []string
	for _, g := range plan.granted {
		roles = append(roles, strings.SplitN(g, ":", 2)[0])
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id := persistence.GenerateID("cpp")
	return id, s.proposalStore.Create(ctx, &persistence.ControlPlaneProposal{
		ID:          id,
		Kind:        persistence.ProposalKindConfig,
		BlastRadius: scope,
		ProjectID:   project,
		Title:       fmt.Sprintf("Let %s of %s use %s tools", strings.Join(roles, ", "), project, server),
		Diff:        diff.String(),
		Rationale: fmt.Sprintf("Filed from the MCP tab's Use in a project page by %s. Tools per role after the change: %s.%s Review the diff, then apply.",
			actor, strings.Join(plan.granted, "; "), shared),
		Evidence:   string(evidence),
		Status:     persistence.ProposalStatusDraft,
		ProposedBy: "operator-ui", // reserved principal, server-stamped
		ApplyOps:   string(rawOps),
	})
}

// yamlMapValue returns the value node under key in mapping m, or nil.
func yamlMapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlMapEnsure returns the value under key, adding an empty node of kind
// when absent.
func yamlMapEnsure(m *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
	if v := yamlMapValue(m, key); v != nil {
		return v
	}
	v := &yaml.Node{Kind: kind}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	return v
}

func encodeYAMLDoc(root *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// editProjectMCPSubscription subscribes the project file to server when it
// is not, and adds tools to a narrowed allowed_tools (U2). An unchanged
// file is returned as it was.
func editProjectMCPSubscription(content []byte, server string, tools []string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("parse project: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("project file is not a mapping")
	}
	doc := root.Content[0]
	servers := yamlMapEnsure(yamlMapEnsure(doc, "mcp", yaml.MappingNode), "servers", yaml.SequenceNode)
	changed := false
	var entry *yaml.Node
	for _, item := range servers.Content {
		if n := yamlMapValue(item, "name"); n != nil && n.Value == server {
			entry = item
		}
	}
	if entry == nil {
		entry = &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "name"}, {Kind: yaml.ScalarNode, Value: server},
		}}
		servers.Content = append(servers.Content, entry)
		changed = true
	} else if allowed := yamlMapValue(entry, "allowed_tools"); allowed != nil && allowed.Kind == yaml.SequenceNode && len(allowed.Content) > 0 {
		// An empty allowed_tools is no narrowing (registry.MCPServerConfig:
		// "When empty, all tools the server advertises are exposed"), so only
		// a non-empty list needs the ticked tools added (code review 3032 F1).
		have := map[string]bool{}
		for _, n := range allowed.Content {
			have[n.Value] = true
		}
		for _, t := range tools {
			if !have[t] {
				allowed.Content = append(allowed.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: t})
				have[t] = true
				changed = true
			}
		}
	}
	if !changed {
		return content, nil
	}
	return encodeYAMLDoc(&root)
}

// editSwarmMCPGrants sets each grid role's part of permissions.allowedTools
// for server to the ticked tools, in the registry's tool order, keeping
// every other entry and its place (U2). A role whose set is unchanged is
// not touched. The result must parse and validate as a swarm. It returns
// the "role: tools" lines of the roles it changed.
func editSwarmMCPGrants(content []byte, filename, server string, data MCPUseData, want map[string]map[string]bool) ([]byte, []string, error) {
	fm, body, err := registry.SplitSwarmContent(content, filename)
	if err != nil {
		return nil, nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(fm, &root); err != nil {
		return nil, nil, fmt.Errorf("parse swarm: %w", err)
	}
	if len(root.Content) == 0 {
		return nil, nil, fmt.Errorf("empty swarm front matter")
	}
	roles := yamlMapValue(root.Content[0], "roles")
	if roles == nil || roles.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("swarm has no roles")
	}
	var granted []string
	for _, role := range data.Roles {
		line, changed, err := setRoleMCPGrant(roles, role, server, data.Tools, want[role.Name])
		if err != nil {
			return nil, nil, err
		}
		if changed {
			granted = append(granted, role.Name+": "+line)
		}
	}
	if len(granted) == 0 {
		return content, nil, nil
	}
	newFM, err := encodeYAMLDoc(&root)
	if err != nil {
		return nil, nil, err
	}
	joined := registry.JoinSwarmContent(newFM, body)
	parsed, err := registry.ParseSwarmMarkdown(joined, filename)
	if err != nil {
		return nil, nil, err
	}
	if err := parsed.Validate(filename); err != nil {
		return nil, nil, err
	}
	return joined, granted, nil
}

// setRoleMCPGrant rewrites one grid role's entries for server in the roles
// sequence to the ticked tools, when they differ from its current grants. It
// returns the role's tools after the change and whether it changed.
func setRoleMCPGrant(roles *yaml.Node, role mcpUseRole, server string, tools []MCPRegistryTool, want map[string]bool) (string, bool, error) {
	if role.Mode != mcpUseGrid {
		return "", false, nil
	}
	same := true
	var names []string
	for _, t := range tools {
		if want[t.Name] != role.Granted[t.Name] {
			same = false
		}
		if want[t.Name] {
			names = append(names, t.Name)
		}
	}
	if same {
		return "", false, nil
	}
	allowed := yamlMapValue(yamlMapValue(swarmRoleNode(roles, role.Name), "permissions"), "allowedTools")
	if allowed == nil || allowed.Kind != yaml.SequenceNode {
		return "", false, fmt.Errorf("role %s has no allowedTools list", role.Name)
	}
	prefix := "mcp__" + server + "__"
	kept := allowed.Content[:0:0]
	for _, n := range allowed.Content {
		if !strings.HasPrefix(n.Value, prefix) || strings.Contains(n.Value, "*") {
			kept = append(kept, n)
		}
	}
	for _, t := range names {
		kept = append(kept, &yaml.Node{Kind: yaml.ScalarNode, Value: mcpQualified(server, t)})
	}
	allowed.Content = kept
	if len(names) == 0 {
		return "none", true, nil
	}
	return strings.Join(names, ", "), true, nil
}

func swarmRoleNode(roles *yaml.Node, name string) *yaml.Node {
	for _, item := range roles.Content {
		if n := yamlMapValue(item, "name"); n != nil && n.Value == name {
			return item
		}
	}
	return nil
}

package agentadmin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"text/template"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/approval"
)

// Renderer renders verbs from the agent templates.
type Renderer struct {
	tmpl *template.Template
}

// Template file names in configs/agent-templates.
const (
	tmplProject  = "project.yaml.tmpl"
	tmplSwarm    = "swarm.md.tmpl"
	tmplWorkflow = "workflow.md.tmpl"
)

// NewRenderer loads the agent templates from fsys (configs/agent-templates).
func NewRenderer(fsys fs.FS) (*Renderer, error) {
	t, err := template.New("").Option("missingkey=error").Funcs(template.FuncMap{
		// q quotes a value as a YAML double-quoted scalar (JSON strings are
		// valid YAML), so no agent text can start a new key or document.
		"q": func(v any) string {
			b, _ := json.Marshal(fmt.Sprint(v))
			return string(b)
		},
	}).ParseFS(fsys, tmplProject, tmplSwarm, tmplWorkflow)
	if err != nil {
		return nil, fmt.Errorf("agent templates: %w", err)
	}
	return &Renderer{tmpl: t}, nil
}

func (r *Renderer) exec(name string, data any) (string, error) {
	var buf bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	return buf.String(), nil
}

// Verbs this package renders.
const (
	VerbCreateProject     = "create_project"
	VerbDefineSwarm       = "define_swarm"
	VerbDefineWorkflow    = "define_workflow"
	VerbAddMCPServer      = "add_mcp_server"
	VerbSetBudget         = "set_budget"
	VerbRemove            = "remove"
	VerbAddAPI            = "add_api"
	VerbRequestCredential = "request_credential"
)

// Render renders one verb call against the namespace's state. A refusal is a
// Change with Class Refused, not an error; an error means a bug or a broken
// template.
func (r *Renderer) Render(st *State, verb string, input json.RawMessage) (Change, error) {
	ns := st.Namespace
	if !agentns.Valid(ns) {
		return Change{}, fmt.Errorf("agentadmin: invalid namespace %q", ns)
	}
	var (
		c   Change
		err error
	)
	switch verb {
	case VerbCreateProject:
		c, err = r.createProject(st, input)
	case VerbDefineSwarm:
		c, err = r.defineSwarm(st, input)
	case VerbDefineWorkflow:
		c, err = r.defineWorkflow(st, input)
	case VerbAddMCPServer:
		c, err = r.addMCPServer(st, input)
	case VerbSetBudget:
		c, err = r.setBudget(st, input)
	case VerbRemove:
		c, err = r.remove(st, input)
	case VerbRequestCredential:
		c = r.requestCredential(st, input)
	case VerbApproveServerTools:
		c, err = r.approveServerTools(st, input)
	case VerbAddAPI:
		c, err = r.addAPI(st, input)
	default:
		return Change{}, ErrUnknownVerb
	}
	if err != nil || c.Class == Refused {
		return c, err
	}
	c.Verb, c.Namespace = verb, ns
	if lock := firstLocked(st, c.Locks); lock != "" {
		return refuse(verb, ns, "%s is part of a change waiting for approval; approve or reject that one first", lock), nil
	}
	if err := attachRendered(&c, input); err != nil {
		return Change{}, err
	}
	return c, nil
}

func firstLocked(st *State, locks []string) string {
	for _, l := range locks {
		if st.Locked[l] {
			return l
		}
	}
	return ""
}

// attachRendered records the canonical typed change the device approves.
func attachRendered(c *Change, input json.RawMessage) error {
	files := make([]string, 0, len(c.Ops))
	for _, op := range c.Ops {
		files = append(files, op.Op+" "+op.Path)
	}
	doc := map[string]any{
		"verb": c.Verb, "namespace": c.Namespace, "input": input,
		"files": files, "grant": c.Grant, "narrowing": c.Narrow,
	}
	if c.Slot != nil {
		doc["slot"] = c.Slot
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	canon, err := approval.Canonical(raw)
	if err != nil {
		return fmt.Errorf("canonical: %w", err)
	}
	c.Rendered = canon
	return nil
}

// Paths, relative to the configs directory.
func projectPath(id string) string  { return "projects/" + id + ".yaml" }
func swarmPath(id string) string    { return "swarms/" + id + ".md" }
func workflowPath(id string) string { return "workflows/" + id + ".md" }

// HashContent is the read-set hash of a file's bytes.
func HashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// expect records the read-set expectation for path: its current hash, or
// absent.
func expect(st *State, rs map[string]string, path string) {
	if h, ok := st.FileHashes[path]; ok {
		rs[path] = h
		return
	}
	rs[path] = ReadSetAbsent
}

// opFor returns create or replace for path, by whether it exists.
func opFor(st *State, path, content string) FileOp {
	if _, ok := st.FileHashes[path]; ok {
		return FileOp{Op: OpReplace, Path: path, Content: content}
	}
	return FileOp{Op: OpCreate, Path: path, Content: content}
}

// projectData is the project template's input.
type projectData struct {
	Namespace, ID, DisplayName, Purpose, SwarmID, DefaultWorkflowID string
	MonthlyUSD                                                      string
	Servers                                                         []ServerState
	APIs                                                            []APIState
	Secrets                                                         []string
}

func (r *Renderer) renderProject(ns string, p *ProjectState) (string, error) {
	servers := append([]ServerState(nil), p.Servers...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	apis := append([]APIState(nil), p.APIs...)
	sort.Slice(apis, func(i, j int) bool { return apis[i].Name < apis[j].Name })
	secrets := append([]string(nil), p.Secrets...)
	sort.Strings(secrets)
	return r.exec(tmplProject, projectData{
		Namespace: ns, ID: p.ID, DisplayName: p.DisplayName, Purpose: p.Purpose,
		SwarmID: p.Swarm.ID, DefaultWorkflowID: p.DefaultWorkflowID,
		MonthlyUSD: formatUSD(p.MonthlyUSD), Servers: servers, APIs: apis, Secrets: secrets,
	})
}

type swarmData struct {
	Namespace, ID, DisplayName, Description, LeadRole, AgentImage string
	Roles                                                         []RoleSpec
}

func (r *Renderer) renderSwarm(st *State, p *ProjectState) (string, error) {
	lead := ""
	if len(p.Swarm.Roles) > 0 {
		lead = p.Swarm.Roles[0].Name
	}
	return r.exec(tmplSwarm, swarmData{
		Namespace: st.Namespace, ID: p.Swarm.ID, DisplayName: p.DisplayName + " roles",
		Description: "Roles of " + p.ID, LeadRole: lead, AgentImage: st.AgentImage, Roles: p.Swarm.Roles,
	})
}

func formatUSD(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	return strings.TrimSuffix(strings.TrimSuffix(s, "0"), ".0")
}

func sortStrings(s []string) { sort.Strings(s) }

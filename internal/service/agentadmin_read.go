package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The read verbs (§6, §10.3, §12). Neither ever carries a credential value,
// and neither names anything outside the caller's namespace.

// SetupView is list_my_setup's answer.
type SetupView struct {
	Namespace  string         `json:"namespace"`
	BudgetUSD  float64        `json:"monthly_budget_total_usd"`
	CeilingUSD float64        `json:"monthly_budget_ceiling_usd"`
	Projects   []SetupProject `json:"projects"`
	Pending    []SetupRequest `json:"awaiting_approval"`
	Failed     []SetupRequest `json:"failed"`
	Expired    []SetupRequest `json:"expired,omitempty"`
}

// SetupProject is one project in list_my_setup.
type SetupProject struct {
	ID          string            `json:"id"`
	Purpose     string            `json:"purpose"`
	BudgetUSD   float64           `json:"monthly_budget_usd"`
	Home        bool              `json:"home,omitempty"`
	Roles       []string          `json:"roles"`
	Workflows   []SetupWorkflow   `json:"workflows"`
	Servers     []SetupServer     `json:"servers"`
	APIs        []SetupAPI        `json:"apis"`
	Credentials []SetupCredential `json:"credentials"`
}

// SetupCredential is one credential slot: its name and whether it is set,
// never its value (plan P4.6).
type SetupCredential struct {
	Name   string     `json:"name"`
	Kind   string     `json:"kind"`   // secret | oauth
	Status string     `json:"status"` // missing | set | needs_reconnect
	SetAt  *time.Time `json:"set_at,omitempty"`
}

// SetupAPI is one REST API and its approval.
type SetupAPI struct {
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url"`
	Status       string   `json:"status"` // approved | not_approved
	ReadMethods  []string `json:"read_methods,omitempty"`
	WriteMethods []string `json:"proposable_write_methods,omitempty"`
}

// SetupWorkflow is one workflow and whether it may run.
type SetupWorkflow struct {
	ID       string         `json:"id"`
	Approved bool           `json:"approved"`
	Schedule *SetupSchedule `json:"schedule,omitempty"`
}

// SetupSchedule is a workflow's schedule (design §17.4): what it is, whether
// it is approved, when it next runs, and its recent slots.
type SetupSchedule struct {
	Cron       string          `json:"cron"`
	Timezone   string          `json:"timezone"`
	Inputs     json.RawMessage `json:"inputs"`
	Sentence   string          `json:"sentence"`
	Approved   bool            `json:"approved"`
	NextRun    *time.Time      `json:"next_run,omitempty"`
	RecentRuns []SetupRun      `json:"recent_runs"`
}

// SetupRun is one past slot: its task, or not_run when nothing ran in it
// (a missed slot, or one before the daemon could fire it).
type SetupRun struct {
	Slot   time.Time `json:"slot"`
	TaskID string    `json:"task_id,omitempty"`
	Status string    `json:"status"`
}

// SetupServer is one integration and its approval.
type SetupServer struct {
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Status     string   `json:"status"` // approved | tools_pending | not_approved
	ReadTools  []string `json:"read_tools,omitempty"`
	WriteTools []string `json:"proposable_write_tools,omitempty"`
}

// SetupRequest is one approval request, as the agent may see it.
type SetupRequest struct {
	ID       string    `json:"id"`
	Sentence string    `json:"sentence"`
	Created  time.Time `json:"created"`
	Reason   string    `json:"reason,omitempty"`
}

// ListSetup answers list_my_setup for an agent admin key.
func (s *agentAdminService) ListSetup(ctx context.Context, key *persistence.APIKey) (SetupView, error) {
	ns := key.AgentNamespace
	st, err := s.loadState(ctx, ns, key.ProjectID)
	if err != nil {
		return SetupView{}, err
	}
	v := SetupView{Namespace: ns, CeilingUSD: st.CeilingUSD, Projects: []SetupProject{}, Pending: []SetupRequest{}, Failed: []SetupRequest{}}
	ids := make([]string, 0, len(st.Projects))
	for id := range st.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := st.Projects[id]
		v.BudgetUSD += p.MonthlyUSD
		sp := SetupProject{ID: id, Purpose: p.Purpose, BudgetUSD: p.MonthlyUSD, Home: p.Home,
			Roles: []string{}, Workflows: []SetupWorkflow{}, Servers: []SetupServer{}, APIs: []SetupAPI{},
			Credentials: s.credentialsOf(ctx, ns, p)}
		for _, r := range p.Swarm.Roles {
			sp.Roles = append(sp.Roles, r.Name)
		}
		sp.Workflows = s.workflowViews(ctx, st, p)
		for _, srv := range p.Servers {
			if strings.HasSuffix(srv.Name, agentns.WriteSuffix) {
				continue // shown as its integration's proposable write tools
			}
			ss := SetupServer{Name: srv.Name, URL: srv.URL, Status: "not_approved"}
			if a, ok := st.Approvals[id][srv.Name]; ok && a.Live() {
				ss.Status, ss.ReadTools, ss.WriteTools = "approved", a.Read, a.Write
				if a.ReadPending {
					ss.Status = "tools_pending"
				}
			}
			sp.Servers = append(sp.Servers, ss)
		}
		for _, api := range p.APIs {
			sa := SetupAPI{Name: api.Name, BaseURL: api.BaseURL, Status: "not_approved"}
			if a, ok := st.Approvals[id][api.Name]; ok && a.Live() {
				sa.Status, sa.ReadMethods, sa.WriteMethods = "approved", a.Read, a.Write
			}
			sp.APIs = append(sp.APIs, sa)
		}
		v.Projects = append(v.Projects, sp)
	}
	return v, s.requestsOf(ctx, ns, &v)
}

func sortedWorkflowIDs(st *agentadmin.State, project string) []string {
	var out []string
	for id := range st.Workflows {
		if agentadmin.ProjectOfWorkflow(id) == project {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// reachMatches reports whether a workflow's live reach is the approved one.
func reachMatches(p *agentadmin.ProjectState, w *agentadmin.WorkflowState) bool {
	sig, err := agentadmin.SignatureOf(p.Loaded, p.LoadedSwarm, w.Loaded)
	return err == nil && sig.Hash() == w.ApprovedReach
}

// requestsOf lists the namespace's open, failed and recently expired
// requests. Expired requests must be re-proposed (§12).
func (s *agentAdminService) requestsOf(ctx context.Context, ns string, v *SetupView) error {
	now := time.Now().UTC()
	pending, err := s.requests.ListPending(ctx, now)
	if err != nil {
		return err
	}
	for _, r := range pending {
		if r.Namespace == ns {
			v.Pending = append(v.Pending, SetupRequest{ID: r.ID, Sentence: r.Sentence, Created: r.CreatedAt})
		}
	}
	recent, err := s.requests.ListRecentByNamespace(ctx, ns, now.Add(-7*24*time.Hour))
	if err != nil {
		return err
	}
	for _, r := range recent {
		switch {
		case r.ApplyError != "":
			v.Failed = append(v.Failed, SetupRequest{ID: r.ID, Sentence: r.Sentence, Created: r.CreatedAt, Reason: r.ApplyError})
		case r.Status == persistence.ApprovalExpired:
			v.Expired = append(v.Expired, SetupRequest{ID: r.ID, Sentence: r.Sentence, Created: r.CreatedAt})
		}
	}
	return nil
}

// Capabilities is describe_installation's answer (§10.3), generated from the
// running configuration, never written by hand.
type Capabilities struct {
	Namespace string `json:"namespace"`
	// HowToWork is the admin guidance (plan P6.3), also served as the
	// companion endpoint's initialize instructions.
	HowToWork     string   `json:"how_to_work"`
	Verbs         []string `json:"verbs"`
	RoleBuiltins  []string `json:"role_builtins"`
	MayNot        []string `json:"may_not"`
	NeedsApproval []string `json:"needs_approval"`
	NotYet        []string `json:"not_available_in_this_release"`
	// Writes says whether workflows may propose writes here (broker.writes).
	Writes string `json:"proposed_writes"`
	// EgressScan states the outbound secret scan (plan P5.5).
	EgressScan    string    `json:"egress_scan"`
	DefaultBudget float64   `json:"default_project_budget_usd"`
	CeilingUSD    float64   `json:"monthly_budget_ceiling_usd"`
	Setup         SetupView `json:"current_setup"`
	// Harness is what the key's harness class means for the guarantees
	// (design §3; plan P6.4).
	Harness HarnessView `json:"harness"`
}

// HarnessView is design §3's row for the key's client kind.
type HarnessView struct {
	ClientKind string   `json:"client_kind"`
	Class      string   `json:"class"`
	Claims     []string `json:"claims"`
}

// Describe answers describe_installation.
func (s *agentAdminService) Describe(ctx context.Context, key *persistence.APIKey) (Capabilities, error) {
	setup, err := s.ListSetup(ctx, key)
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{
		Namespace: key.AgentNamespace,
		HowToWork: agentadmin.AdminGuidance(),
		Verbs: []string{agentadmin.VerbCreateProject, agentadmin.VerbDefineSwarm, agentadmin.VerbDefineWorkflow,
			agentadmin.VerbAddMCPServer, agentadmin.VerbAddAPI, agentadmin.VerbRequestCredential,
			agentadmin.VerbSetBudget, agentadmin.VerbRemove, "list_my_setup", "describe_installation"},
		RoleBuiltins: append(registry.BrokerSafeBuiltins(), agentadmin.QueryAPITool+" (read only, once the project has an approved API)"),
		MayNot: []string{
			"see or enter a credential value", "approve anything", "reach a URL that is not https (plain http only to this machine)",
			"run a program on this machine", "touch anything outside namespace " + key.AgentNamespace,
			"change a model", "remove a budget or make it unlimited",
		},
		NeedsApproval: []string{
			"connecting a server or an API", "every credential (the user enters it, or signs in, on their phone)",
			"a new workflow, or any change to what a workflow returns or can reach",
			"raising a budget, or exceeding the namespace ceiling",
			"a schedule (when a workflow runs by itself, in which timezone, with which inputs), or any change to it",
		},
		NotYet: []string{},
		Writes: s.writesState(),
		EgressScan: "Tool arguments, API requests, proposed writes and returned documents are scanned for credential-shaped values " +
			"(keys, tokens); a finding refuses the call and names the field, never the value. Personal data in general is not detected.",
		DefaultBudget: s.c.Config.AgentAdmin.EffectiveDefaultProjectBudget(),
		CeilingUSD:    setup.CeilingUSD,
		Setup:         setup,
		Harness:       harnessView(key.ClientKind),
	}, nil
}

// harnessView classifies a client kind with the predicate vornikctl agent
// connect also uses (plan P6 amendment F8).
func harnessView(clientKind string) HarnessView {
	class := agentadmin.HarnessClassOf(clientKind)
	return HarnessView{ClientKind: clientKind, Class: class, Claims: agentadmin.HarnessClaims(class)}
}

// workflowViews is one project's workflows: approval, and the schedule
// when there is one (design §17.4).
func (s *agentAdminService) workflowViews(ctx context.Context, st *agentadmin.State, p *agentadmin.ProjectState) []SetupWorkflow {
	out := []SetupWorkflow{}
	for _, wid := range sortedWorkflowIDs(st, p.ID) {
		w := st.Workflows[wid]
		sw := SetupWorkflow{ID: wid, Approved: w.ApprovedReach != "" && reachMatches(p, w)}
		if w.Loaded != nil && w.Loaded.Broker != nil && w.Loaded.Broker.Schedule != nil {
			sw.Schedule = s.scheduleView(ctx, p.ID, wid, w.Loaded.Broker.Schedule, sw.Approved, time.Now())
		}
		out = append(out, sw)
	}
	return out
}

// ListSetupJSON adapts ListSetup to the API's interface.
func (s *agentAdminService) ListSetupJSON(ctx context.Context, key *persistence.APIKey) (any, error) {
	return s.ListSetup(ctx, key)
}

// DescribeJSON adapts Describe to the API's interface.
func (s *agentAdminService) DescribeJSON(ctx context.Context, key *persistence.APIKey) (any, error) {
	return s.Describe(ctx, key)
}

// EnsureHome creates the namespace's home project through the inert path,
// as the system, if it does not exist yet (plan P3.1).
func (s *agentAdminService) EnsureHome(ctx context.Context, ns, clientKind string) (string, error) {
	home := agentns.ID(ns, "home")
	if s.c.Registry.GetProject(home) != nil {
		return home, nil
	}
	key := &persistence.APIKey{ID: "grant:" + ns, ClientKind: clientKind, AgentAdmin: true, AgentNamespace: ns, ProjectID: home}
	raw, _ := json.Marshal(agentadmin.CreateProjectInput{Slug: "home", Purpose: "Home of the " + ns + " assistant"})
	res, err := s.Do(ctx, key, agentadmin.VerbCreateProject, raw)
	if err != nil {
		return "", err
	}
	if res.Effect != agentadmin.EffectApplied {
		return "", fmt.Errorf("the home project %s could not be created: %s", home, res.Reason)
	}
	return home, nil
}

// credentialsOf lists a project's credential slots with their status: typed
// secrets from the store's metadata, OAuth slots from the token row. Never a
// value; a store that does not exist yet means every secret is missing.
func (s *agentAdminService) credentialsOf(ctx context.Context, ns string, p *agentadmin.ProjectState) []SetupCredential {
	out := []SetupCredential{}
	setAt := map[string]time.Time{}
	if st := s.c.currentSecretSource().Store; st != nil {
		if metas, err := st.List(ctx, ns); err == nil {
			for _, m := range metas {
				setAt[m.Name] = m.UpdatedAt
			}
		}
	}
	for _, ref := range p.Secrets {
		name := strings.TrimPrefix(ref, ns+"/")
		c := SetupCredential{Name: name, Kind: agentadmin.CredentialSecret, Status: "missing"}
		if at, ok := setAt[name]; ok {
			c.Status, c.SetAt = "set", &at
		}
		out = append(out, c)
	}
	conn := s.c.mcpConnector()
	for _, srv := range p.Servers {
		if !srv.OAuth || strings.HasSuffix(srv.Name, agentns.WriteSuffix) {
			continue
		}
		c := SetupCredential{Name: agentadmin.OAuthCredentialName(srv.Name), Kind: agentadmin.CredentialOAuth, Status: "missing"}
		if conn != nil {
			if tok, err := conn.Tokens.Get(ctx, p.ID, srv.Name); err == nil {
				at := tok.ConnectedAt
				c.Status, c.SetAt = "set", &at
				if tok.NeedsReconnect {
					c.Status = "needs_reconnect"
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// writesState states whether workflows may propose writes (plan P4.8).
func (s *agentAdminService) writesState() string {
	if mode, err := s.c.Config.Broker.WritesMode(); err == nil && mode == "on" {
		return "on: a workflow may propose writes; the user approves each one on their phone"
	}
	return "off on this installation (the operator's broker.writes setting); workflows cannot propose writes"
}

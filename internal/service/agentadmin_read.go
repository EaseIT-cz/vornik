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
	"vornik.io/vornik/internal/agenttools"
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
	// Notifications are advisory terminal approval signals since the last
	// list_my_setup call for this namespace. The approval rows in this same
	// response remain authoritative.
	Notifications []SetupNotification `json:"notifications,omitempty"`
	// ApprovedModelDestinations are the remote model destinations this
	// namespace's approver approved and the operator has not withdrawn
	// (design §18.14 finding 1): live rows only.
	ApprovedModelDestinations []SetupModelDestination `json:"approved_model_destinations"`
}

// SetupNotification is one content-free terminal approval signal for an
// agent-admin client. It never carries rendered request content or credential
// values.
type SetupNotification struct {
	ChangeID string `json:"change_id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
}

// SetupModelDestination is one approved model destination, exactly
// {destination, approved_at} (design §18.14 round 2 F7).
type SetupModelDestination struct {
	Destination string    `json:"destination"`
	ApprovedAt  time.Time `json:"approved_at"`
}

// SetupRole is one role of a project's swarm as list_my_setup shows it
// (design §18.14 finding 1, round 2 F6): its model (absent for the
// operator's default), its granted tools, and where it runs now, judged as
// the executor judges it before every attempt (agentadmin.RoleRunsOn).
type SetupRole struct {
	Name   string   `json:"name"`
	Model  string   `json:"model,omitempty"`
	Tools  []string `json:"tools"`
	RunsOn string   `json:"runs_on"`
}

// SetupProject is one project in list_my_setup.
type SetupProject struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Purpose     string   `json:"purpose"`
	BudgetUSD   float64  `json:"monthly_budget_usd"`
	Home        bool     `json:"home,omitempty"`
	Roles       []string `json:"roles"`
	// RoleDetails is Roles with each role's model, tools and where it runs
	// (design §18.14); Roles stays as it was so nothing that reads it breaks.
	RoleDetails []SetupRole       `json:"role_details"`
	Workflows   []SetupWorkflow   `json:"workflows"`
	Servers     []SetupServer     `json:"servers"`
	APIs        []SetupAPI        `json:"apis"`
	Credentials []SetupCredential `json:"credentials"`
	// NextSteps says what stands between the project and a runnable setup
	// (design §19.2, §19.7 F2): a credential to enter, or one declined.
	NextSteps []string `json:"next_steps,omitempty"`
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
	// Note is why a server's tools were not filed for approval (design
	// §19.8 F6: it does not offer a recipe's tools).
	Note string `json:"note,omitempty"`
}

// SetupRequest is one approval request, as the agent may see it.
type SetupRequest struct {
	ID       string    `json:"id"`
	Sentence string    `json:"sentence"`
	Created  time.Time `json:"created"`
	Reason   string    `json:"reason,omitempty"`
	// ReRenderedAs, on a failed request, names the request the daemon filed
	// in its place with the true figures; ReRenderOf, on that request, names
	// the one it replaces (design §18.4 item 4).
	ReRenderedAs string `json:"re_rendered_as,omitempty"`
	ReRenderOf   string `json:"re_render_of,omitempty"`
}

// ListSetup answers list_my_setup for an agent admin key.
func (s *agentAdminService) ListSetup(ctx context.Context, key *persistence.APIKey) (SetupView, error) {
	ns := key.AgentNamespace
	st, err := s.loadState(ctx, ns, key.ProjectID)
	if err != nil {
		return SetupView{}, err
	}
	v := SetupView{Namespace: ns, CeilingUSD: st.CeilingUSD, Projects: []SetupProject{}, Pending: []SetupRequest{}, Failed: []SetupRequest{}}
	// runs_on and approved_model_destinations come from ONE read of the
	// approval rows, so a withdrawal landing during this call cannot make
	// the answer contradict itself (review 20261003-195e F1). The rows are
	// agent_model_provider_approvals with removed_at unset, the same rows
	// the executor's run-time check reads (verifyAgentModel, through
	// GetModelDestination) before every attempt.
	if v.ApprovedModelDestinations, err = s.approvedModelDestinations(ctx, ns); err != nil {
		return SetupView{}, err
	}
	live := map[string]bool{}
	for _, d := range v.ApprovedModelDestinations {
		live[d.Destination] = true
	}
	specs := s.c.agentModelSpecs()
	approved := func(dest string) bool { return live[dest] }
	ids := make([]string, 0, len(st.Projects))
	for id := range st.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := st.Projects[id]
		v.BudgetUSD += p.MonthlyUSD
		sp := SetupProject{ID: id, DisplayName: p.DisplayName, Purpose: p.Purpose, BudgetUSD: p.MonthlyUSD, Home: p.Home,
			Roles: []string{}, RoleDetails: []SetupRole{}, Workflows: []SetupWorkflow{}, Servers: []SetupServer{}, APIs: []SetupAPI{},
			Credentials: s.credentialsOf(ctx, ns, p)}
		for _, r := range p.Swarm.Roles {
			sp.Roles = append(sp.Roles, r.Name)
			sp.RoleDetails = append(sp.RoleDetails, SetupRole{Name: r.Name, Model: r.Model, Tools: append([]string{}, r.Tools...),
				RunsOn: agentadmin.RoleRunsOn(r.Model, specs, s.c.modelRoute, approved)})
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
					ss.Note = s.toolGap(id, srv.Name)
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
		s.credentialSteps(ctx, ns, &sp)
		v.Projects = append(v.Projects, sp)
	}
	if err := s.requestsOf(ctx, ns, &v); err != nil {
		return SetupView{}, err
	}
	v.Notifications = s.drainNotifications(ns)
	return v, nil
}

// approvedModelDestinations lists the namespace's live model destination
// approvals with when each was approved; a withdrawn one is absent (design
// §18.14 finding 1).
func (s *agentAdminService) approvedModelDestinations(ctx context.Context, ns string) ([]SetupModelDestination, error) {
	out := []SetupModelDestination{}
	rows, err := s.c.repos.AgentGrants.ListModelDestinations(ctx, ns)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.RemovedAt == nil {
			out = append(out, SetupModelDestination{Destination: r.Destination, ApprovedAt: r.ApprovedAt.UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Destination < out[j].Destination })
	return out, nil
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
	replacedBy := map[string]string{} // failed request → the one filed in its place (§18.4)
	for _, r := range pending {
		if r.Namespace == ns {
			sr := SetupRequest{ID: r.ID, Sentence: r.Sentence, Created: r.CreatedAt}
			var pl approvalPayload
			if r.Kind == persistence.ApprovalKindWideningChange && json.Unmarshal(r.Rendered, &pl) == nil && pl.ReRenderOf != "" {
				sr.ReRenderOf = pl.ReRenderOf
				replacedBy[pl.ReRenderOf] = r.ID
			}
			v.Pending = append(v.Pending, sr)
		}
	}
	recent, err := s.requests.ListRecentByNamespace(ctx, ns, now.Add(-7*24*time.Hour))
	if err != nil {
		return err
	}
	for _, r := range recent {
		switch {
		case r.ApplyError != "":
			v.Failed = append(v.Failed, SetupRequest{ID: r.ID, Sentence: r.Sentence, Created: r.CreatedAt, Reason: r.ApplyError, ReRenderedAs: replacedBy[r.ID]})
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
	// DaemonVersion is the build answering, read on every call (design
	// §18.14 finding 2, review be5a), for the agent's information only.
	DaemonVersion string `json:"daemon_version"`
	// HowToWork is the admin guidance (plan P6.3), also served as the
	// companion endpoint's initialize instructions.
	HowToWork    string   `json:"how_to_work"`
	Verbs        []string `json:"verbs"`
	RoleBuiltins []string `json:"role_builtins"`
	// AlwaysGranted are the tools every role can call beyond those it
	// declares (agenttools.EveryRole): they read only the task's own project
	// memory and skills, search the step's own tool catalogue, and re-read
	// the step's own tool output (design §18.8).
	AlwaysGranted []string `json:"always_granted"`
	MayNot        []string `json:"may_not"`
	NeedsApproval []string `json:"needs_approval"`
	NotYet        []string `json:"not_available_in_this_release"`
	// InputRules are the rules a broker workflow's inputs schema must follow,
	// from the validator's own source (design §18.2), so an agent can read
	// them before it writes a schema.
	InputRules []registry.BrokerInputRule `json:"input_rules"`
	// Documents states the document input (broker design §18.5, §18.7 F5):
	// its caps, where the role finds it, and that each reading step pays.
	Documents string `json:"documents"`
	// OnePendingChangePerProject: while one change to a project waits for
	// approval, other changes to that project are refused (design §18.2).
	OnePendingChangePerProject bool `json:"one_pending_change_per_project"`
	// Writes says whether workflows may propose writes here (broker.writes).
	Writes string `json:"proposed_writes"`
	// EgressScan states the outbound secret scan (plan P5.5).
	EgressScan    string  `json:"egress_scan"`
	DefaultBudget float64 `json:"default_project_budget_usd"`
	CeilingUSD    float64 `json:"monthly_budget_ceiling_usd"`
	// Models are the models a role may name in define_swarm (design §18.6
	// item 2); empty when the operator offers none.
	Models []DescribedModel `json:"models"`
	// ModelsNote says what a role with no model runs on (review
	// 20261003-2ed0 B5).
	ModelsNote string    `json:"models_note"`
	Setup      SetupView `json:"current_setup"`
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

// DescribedModel is one model of the installation's catalogue as
// describe_installation shows it (design §18.6 item 2, Change 3): what it
// is good for, where it sends a role's work, and its price.
type DescribedModel struct {
	ID      string `json:"id"`
	GoodFor string `json:"good_for"`
	// Where is "local" or "remote: <sub-provider> at <host>".
	Where string `json:"where"`
	// Price is per million input and output tokens, or "no charge
	// recorded" for an unpriced local model.
	Price string `json:"price"`
}

// describeModels lists the catalogue, classified against the live router.
func (s *agentAdminService) describeModels() []DescribedModel {
	cat, _ := s.c.agentModelCatalogue()
	out := []DescribedModel{}
	for _, m := range agentadmin.SortedModels(cat) {
		out = append(out, DescribedModel{ID: m.ID, GoodFor: m.GoodFor, Where: m.Where(), Price: m.PriceLabel()})
	}
	return out
}

// Describe answers describe_installation.
func (s *agentAdminService) Describe(ctx context.Context, key *persistence.APIKey) (Capabilities, error) {
	setup, err := s.ListSetup(ctx, key)
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{
		Namespace:     key.AgentNamespace,
		DaemonVersion: s.c.Version(),
		HowToWork:     agentadmin.AdminGuidance(),
		Verbs: []string{agentadmin.VerbCreateProject, agentadmin.VerbDefineSwarm, agentadmin.VerbDefineWorkflow,
			agentadmin.VerbAddMCPServer, agentadmin.VerbAddAPI, agentadmin.VerbRequestCredential,
			agentadmin.VerbSetBudget, agentadmin.VerbUpdateProject, agentadmin.VerbRemove, agentadmin.VerbInstallRecipe, agentadmin.VerbListRecipes,
			"list_my_setup", "describe_installation"},
		RoleBuiltins:  append(registry.BrokerSafeBuiltins(), agentadmin.QueryAPITool+" (read only, once the project has an approved API)"),
		AlwaysGranted: agenttools.EveryRole(),
		MayNot: []string{
			"see or enter a credential value", "approve anything", "reach a URL that is not https (plain http only to this machine)",
			"run a program on this machine", "touch anything outside namespace " + key.AgentNamespace,
			"choose a model outside the installation's catalogue (models lists it)", "give a role a fallback model", "remove a budget or make it unlimited",
		},
		NeedsApproval: []string{
			"connecting a server or an API", "every credential (the user enters it, or signs in, on their phone)",
			"a new workflow, or any change to what a workflow returns or can reach",
			"raising a budget, or exceeding the namespace ceiling",
			"a schedule (when a workflow runs by itself, in which timezone, with which inputs), or any change to it",
			"a role on a remote model whose destination (provider and host) this namespace has not used before; a local model, or a destination already approved, applies at once",
		},
		Models: s.describeModels(),
		ModelsNote: "A role may name one of models; a remote one needs the user's approval once per destination. " +
			"A role with no model runs on the operator's default model, which may be remote: what it works on goes wherever the operator's configuration sends it.",
		NotYet:                     []string{},
		InputRules:                 registry.BrokerInputRules(),
		Documents:                  documentsStatement(),
		OnePendingChangePerProject: true,
		Writes:                     s.writesState(),
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
	return HarnessView{ClientKind: clientKind, Class: class, Claims: agentadmin.HarnessClaimsFor(clientKind)}
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
// ApprovedWorkflows is every approved workflow of the key's namespace,
// sorted by project then workflow: the same approval list_my_setup reports,
// so catalog and list_my_setup cannot disagree (design §18.3).
func (s *agentAdminService) ApprovedWorkflows(ctx context.Context, key *persistence.APIKey) ([]string, error) {
	if err := s.coverOnRead(ctx, key); err != nil {
		return nil, err
	}
	v, err := s.ListSetup(ctx, key)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, p := range v.Projects {
		for _, w := range p.Workflows {
			if w.Approved {
				ids = append(ids, w.ID)
			}
		}
	}
	return ids, nil
}

func (s *agentAdminService) ListSetupJSON(ctx context.Context, key *persistence.APIKey) (any, error) {
	if err := s.coverOnRead(ctx, key); err != nil {
		return nil, err
	}
	return s.ListSetup(ctx, key)
}

// DescribeJSON adapts Describe to the API's interface.
func (s *agentAdminService) DescribeJSON(ctx context.Context, key *persistence.APIKey) (any, error) {
	if err := s.coverOnRead(ctx, key); err != nil {
		return nil, err
	}
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

// documentsStatement is describe_installation's statement of the document
// input (broker design §18.5, §18.7 F5), from the registry's own bounds.
func documentsStatement() string {
	return fmt.Sprintf("A workflow may declare up to %d top-level x-untrusted-document inputs (text/plain, text/markdown or text/x-diff), "+
		"each at most %s bytes and %s bytes together. You pass the text as the input's value in delegate. "+
		"Each step finds it as a read-only file at artifacts/in/<property>.<ext>; it is never put into the prompt and never returned. "+
		"Each step that reads it pays for it in tokens (a 256 KiB document is about 64k tokens per reading step), "+
		"and a step whose model cannot hold it fails as any oversized prompt does. "+
		"A document can steer the team: the most it can reach is a read from a connection the user approved or a write proposal the user must approve.",
		registry.BrokerDocumentsMax, groupDigits(registry.BrokerDocumentMaxBytes), groupDigits(registry.BrokerDocumentsTotalMaxBytes))
}

// groupDigits writes n with comma thousands separators.
func groupDigits(n int) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

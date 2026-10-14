package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Read-only connectors a paired device installs (agent-administered design
// §19.14, build decisions §19.14.8; GitHub #314). The device page lists the
// vetted connectors, renders the install's sentence for the person's
// choice, and on one confirm runs, in this order: the install (the same
// widening_change effect, decided by the device), the credential (the same
// credential_slot decide-then-store, without the agent path's detached
// tools listing), the tools approval (approve_server_tools with exactly
// the recipe's read tools, decided by the device), then the connection
// check. A step that fails stops the ones after it, so the tools are never
// approved before the credential is stored.

// Connector confirm steps, in order (the test seam sees each).
const (
	connectorStepInstall    = "install"
	connectorStepCredential = "credential"
	connectorStepTools      = "tools"
	connectorStepCheck      = "check"
)

// connectorActorKind is the ledger's actor kind for a device install; the
// device's id is the proposal's actor credential.
const connectorActorKind = "device"

// connectorProposer is who proposes a device install's changes in the
// ledger: the vetted connector, whose catalogue entry is every byte of the
// change. The device approves, and the ledger forbids a proposer approving
// its own proposal (persistence.ErrProposalSelfApprove), so the device is
// recorded as the actor and the approver, never as the proposer.
func connectorProposer(name string) string { return "connector:" + name }

// connectorPages implements approverdevice.ConnectorPages.
type connectorPages struct{ s *agentAdminService }

// lazyConnectorPages resolves the agent admin service on each call, so the
// page works once the agent templates are installed (see agentAdmin).
type lazyConnectorPages struct{ c *Container }

func (l lazyConnectorPages) pages() (connectorPages, error) {
	s := l.c.agentAdmin()
	if s == nil {
		return connectorPages{}, agentadmin.ErrUnavailable
	}
	return connectorPages{s: s}, nil
}

func (l lazyConnectorPages) List(ctx context.Context) ([]approverdevice.ConnectorView, error) {
	p, err := l.pages()
	if err != nil {
		return nil, err
	}
	return p.List(ctx)
}

func (l lazyConnectorPages) Namespaces(ctx context.Context) ([]approverdevice.ConnectorNamespace, error) {
	p, err := l.pages()
	if err != nil {
		return nil, err
	}
	return p.Namespaces(ctx)
}

func (l lazyConnectorPages) Preview(ctx context.Context, name string, ch approverdevice.ConnectorChoice) (approverdevice.ConnectorPreview, error) {
	p, err := l.pages()
	if err != nil {
		return approverdevice.ConnectorPreview{}, err
	}
	return p.Preview(ctx, name, ch)
}

func (l lazyConnectorPages) CreateProject(ctx context.Context, d *approverdevice.Device, name, ns string) error {
	p, err := l.pages()
	if err != nil {
		return err
	}
	return p.CreateProject(ctx, d, name, ns)
}

func (l lazyConnectorPages) Confirm(ctx context.Context, d *approverdevice.Device, name string, ch approverdevice.ConnectorChoice, shown string, creds map[string][]byte) (approverdevice.ConnectorOutcome, error) {
	p, err := l.pages()
	if err != nil {
		return approverdevice.ConnectorOutcome{}, err
	}
	return p.Confirm(ctx, d, name, ch, shown, creds)
}

func (l lazyConnectorPages) Pending(ctx context.Context, name string, ch approverdevice.ConnectorChoice) (approverdevice.ConnectorPending, error) {
	p, err := l.pages()
	if err != nil {
		return approverdevice.ConnectorPending{}, err
	}
	return p.Pending(ctx, name, ch)
}

func (l lazyConnectorPages) SignIn(ctx context.Context, d *approverdevice.Device, name string, ch approverdevice.ConnectorChoice, server string) (approverdevice.ConnectorOutcome, error) {
	p, err := l.pages()
	if err != nil {
		return approverdevice.ConnectorOutcome{}, err
	}
	return p.SignIn(ctx, d, name, ch, server)
}

func (l lazyConnectorPages) Finish(ctx context.Context, d *approverdevice.Device, name string, ch approverdevice.ConnectorChoice, shown string) (approverdevice.ConnectorOutcome, error) {
	p, err := l.pages()
	if err != nil {
		return approverdevice.ConnectorOutcome{}, err
	}
	return p.Finish(ctx, d, name, ch, shown)
}

// List is the vetted connectors with what each reads and returns, and
// every workflow installed from it in any namespace.
func (p connectorPages) List(_ context.Context) ([]approverdevice.ConnectorView, error) {
	s := p.s
	installs := map[string][]string{}
	for _, wf := range s.c.Registry.ListWorkflows() {
		if _, ok := agentns.FromID(wf.ID); !ok {
			continue
		}
		if name, _ := agentadmin.InstalledRecipe(wf); name != "" {
			installs[name] = append(installs[name], wf.ID)
		}
	}
	var out []approverdevice.ConnectorView
	for _, rec := range s.renderer.Catalogue().Connectors() {
		v := approverdevice.ConnectorView{Name: rec.Name, Title: rec.Connector.Title, ReturnsPlain: rec.Returns,
			Returns: rec.ReturnsLines(), Installed: installs[rec.Name]}
		sort.Strings(v.Installed)
		for _, n := range rec.Needs {
			tools := append([]string(nil), n.ReadTools...)
			sort.Strings(tools)
			v.Reads = append(v.Reads, approverdevice.ConnectorRead{Server: n.Name, Reads: n.Reads,
				Host: hostOfURL(rec.Connector.Servers[n.Name].URL), Tools: tools})
		}
		for _, dv := range rec.DeviceVariables() {
			cv := approverdevice.ConnectorVariable{Name: dv.Name, Type: dv.Type, Help: dv.Help, Values: dv.Values}
			if dv.Default != nil {
				cv.Default = *dv.Default
			}
			v.Variables = append(v.Variables, cv)
		}
		out = append(out, v)
	}
	return out, nil
}

func hostOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// Namespaces is every agent namespace with a project, and its projects
// except home, which is the assistant's own scratch.
func (p connectorPages) Namespaces(_ context.Context) ([]approverdevice.ConnectorNamespace, error) {
	byNS := map[string][]string{}
	for _, pr := range p.s.c.Registry.ListProjects() {
		if pr == nil {
			continue
		}
		ns, ok := agentns.FromID(pr.ID)
		if !ok {
			continue
		}
		slug := strings.TrimPrefix(pr.ID, ns+agentns.Separator)
		if _, seen := byNS[ns]; !seen {
			byNS[ns] = []string{}
		}
		if slug != "home" {
			byNS[ns] = append(byNS[ns], slug)
		}
	}
	out := make([]approverdevice.ConnectorNamespace, 0, len(byNS))
	for ns, projects := range byNS {
		sort.Strings(projects)
		out = append(out, approverdevice.ConnectorNamespace{Name: ns, Projects: projects})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// namespaceExists reports whether ns is an agent namespace with a project.
func (p connectorPages) namespaceExists(ctx context.Context, ns string) bool {
	if !agentns.Valid(ns) {
		return false
	}
	all, _ := p.Namespaces(ctx)
	for _, n := range all {
		if n.Name == ns {
			return true
		}
	}
	return false
}

// shownOf binds a confirm to the change its page showed: the canonical
// rendered change and the sentence (design §19.14.8 item 5c).
func shownOf(ch agentadmin.Change) string {
	h := sha256.New()
	h.Write(ch.Rendered)
	h.Write([]byte{0})
	h.Write([]byte(ch.Sentence))
	return hex.EncodeToString(h.Sum(nil))
}

// connectorRender is a device install rendered for a choice.
type connectorRender struct {
	rec     *agentadmin.Recipe
	ch      agentadmin.Change
	refused string
	missing bool // the project does not exist yet
}

// renderConnector renders the install a choice makes, from the catalogue
// only. The caller holds s.mu.
func (p connectorPages) renderConnector(ctx context.Context, name string, c approverdevice.ConnectorChoice) (connectorRender, error) {
	s := p.s
	rec := s.renderer.Catalogue().Get(name)
	out := connectorRender{rec: rec}
	if !rec.IsConnector() {
		// Re-read at every render, so a connector demoted since the page
		// was shown is refused here (review-20261013-99c9 F4).
		out.refused = fmt.Sprintf("%q is not a connector this device can install now", name)
		return out, nil
	}
	if !p.namespaceExists(ctx, c.Namespace) {
		out.refused = "choose one of your assistants"
		return out, nil
	}
	slug := agentadmin.ProjectSlug(c.Namespace, c.Project)
	if s.c.Registry.GetProject(agentns.ID(c.Namespace, slug)) == nil {
		if slug == name {
			out.missing = true
			return out, nil
		}
		out.refused = fmt.Sprintf("there is no project %q in %s", slug, c.Namespace)
		return out, nil
	}
	raw, why := s.renderer.ConnectorInstallInput(name, slug, c.Account, c.Variables)
	if why != "" {
		out.refused = why
		return out, nil
	}
	st, err := s.loadState(ctx, c.Namespace, agentns.ID(c.Namespace, "home"))
	if err != nil {
		return out, err
	}
	ch, err := s.renderer.Render(st, agentadmin.VerbInstallRecipe, raw)
	if err != nil {
		return out, err
	}
	switch {
	case ch.Class == agentadmin.Refused:
		out.refused = ch.Reason
	case ch.Class != agentadmin.Widening:
		out.refused = ch.Sentence
	}
	out.ch = ch
	return out, nil
}

// Preview renders the sentence a choice's confirm approves.
func (p connectorPages) Preview(ctx context.Context, name string, c approverdevice.ConnectorChoice) (approverdevice.ConnectorPreview, error) {
	s := p.s
	if s.renderer.Catalogue().Get(name) == nil {
		return approverdevice.ConnectorPreview{}, approverdevice.ErrNotAConnector
	}
	s.mu.Lock()
	r, err := p.renderConnector(ctx, name, c)
	s.mu.Unlock()
	if err != nil {
		return approverdevice.ConnectorPreview{}, err
	}
	switch {
	case r.missing:
		return approverdevice.ConnectorPreview{ProjectMissing: true}, nil
	case r.refused != "":
		return approverdevice.ConnectorPreview{Refused: r.refused}, nil
	}
	pv := approverdevice.ConnectorPreview{Sentence: r.ch.Sentence, Shown: shownOf(r.ch)}
	for _, f := range r.ch.Credentials {
		if f.Kind == agentadmin.CredentialOAuth {
			pv.SignInAfter = true // no typed value: a sign-in follows (§19.14.10)
			continue
		}
		pv.Credentials = append(pv.Credentials, approverdevice.ConnectorCredential{Name: f.Name,
			Stored: s.credentialPresent(ctx, c.Namespace, f.Project, f.Name, f.Kind)})
	}
	return pv, nil
}

// CreateProject creates the connector's own project in ns (design §19.14.8
// item 5a): create_project, inert as an agent's is, proposed by the device.
func (p connectorPages) CreateProject(ctx context.Context, d *approverdevice.Device, name, ns string) error {
	s := p.s
	if d == nil {
		return approverdevice.ErrNoDevice
	}
	rec := s.renderer.Catalogue().Get(name)
	if !rec.IsConnector() {
		return approverdevice.ErrNotAConnector
	}
	if !p.namespaceExists(ctx, ns) {
		return errors.New("choose one of your assistants")
	}
	raw, _ := json.Marshal(agentadmin.CreateProjectInput{Slug: name, Purpose: rec.Connector.Title})
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadState(ctx, ns, agentns.ID(ns, "home"))
	if err != nil {
		return err
	}
	ch, err := s.renderer.Render(st, agentadmin.VerbCreateProject, raw)
	if err != nil {
		return err
	}
	if ch.Class != agentadmin.Inert {
		if ch.Reason != "" {
			return errors.New(ch.Reason)
		}
		return errors.New("the project cannot be created from the device")
	}
	prop, err := s.fileProposalBy(ctx, connectorProposer(name), connectorActorKind, d.ID, ch)
	if err != nil {
		return err
	}
	res, err := s.applyInert(ctx, prop, ch)
	if err != nil {
		return err
	}
	if res.Effect != EffectApplied {
		return errors.New(res.Reason)
	}
	return nil
}

// step runs the test seam for a step; nil in production.
func (s *agentAdminService) step(name string) error {
	if s.connectorStep == nil {
		return nil
	}
	return s.connectorStep(name)
}

// Confirm is the one submission (design §19.14.2 item 3, §19.14.8 item 5).
func (p connectorPages) Confirm(ctx context.Context, d *approverdevice.Device, name string, c approverdevice.ConnectorChoice, shown string, creds map[string][]byte) (approverdevice.ConnectorOutcome, error) {
	s := p.s
	if d == nil {
		return approverdevice.ConnectorOutcome{}, approverdevice.ErrNoDevice
	}
	install, refused, err := p.fileInstall(ctx, d, name, c, shown, creds)
	if err != nil || refused.Refused != "" {
		return refused, err
	}
	out := approverdevice.ConnectorOutcome{}
	fail := func(step string, why string) (approverdevice.ConnectorOutcome, error) {
		out.Failed, out.FailedWhy = step, why
		return out, nil
	}
	// Step d: the install, decided by the device; applyApproved runs as for
	// any widening change. Its payload carries no credential follow-ups.
	if err := s.step(connectorStepInstall); err != nil {
		return fail(connectorStepInstall, err.Error())
	}
	if err := s.devices.Decide(ctx, d, install.row.ID, install.row.RenderedSHA256, true); err != nil {
		return fail(connectorStepInstall, "the install could not be applied: "+err.Error())
	}
	out.Installed = true
	for wid := range install.ch.Grant.Workflows {
		row := persistence.AgentConnectorInstall{WorkflowID: wid, Namespace: c.Namespace, ProjectID: agentadmin.ProjectOfWorkflow(wid),
			Connector: name, AccountLabel: agentns.AccountOf(c.Account), DeviceID: d.ID, InstalledAt: s.now().UTC()}
		if err := s.grants.UpsertConnectorInstall(ctx, row); err != nil {
			// Attribution only: the install stands (review-20261013-f1f5 N4).
			s.c.Logger.Warn().Err(err).Str("workflow", wid).Msg("agent admin: recording a device's connector install failed")
		}
	}
	// Step e: each credential typed on the form, decided and stored.
	if err := s.step(connectorStepCredential); err != nil {
		return fail(connectorStepCredential, err.Error())
	}
	for _, f := range install.ch.Credentials {
		value := creds[f.Name]
		if f.Kind == agentadmin.CredentialOAuth || len(value) == 0 {
			continue // a sign-in follows; or already stored, checked before anything was filed
		}
		if why := p.storeCredential(ctx, d, c.Namespace, f, value); why != "" {
			return fail(connectorStepCredential, why)
		}
	}
	// Step f: exactly the recipe's read tools for each server that waits and
	// whose credential is now stored. An OAuth server waits for its sign-in
	// and the second confirm (design §19.14.10).
	if err := s.step(connectorStepTools); err != nil {
		return fail(connectorStepTools, err.Error())
	}
	checked, signIn, why := p.approveReady(ctx, d, name, c.Namespace, install.ch.Grant.Integrations)
	if why != "" {
		return fail(connectorStepTools, why)
	}
	// Step g: the connection check, as test_connection runs it.
	if err := s.step(connectorStepCheck); err != nil {
		return fail(connectorStepCheck, err.Error())
	}
	pid := agentns.ID(c.Namespace, agentadmin.ProjectSlug(c.Namespace, c.Project))
	for _, server := range checked {
		out.Checks = append(out.Checks, approverdevice.ConnectorCheck{Server: server, Code: p.check(ctx, d, pid, server)})
	}
	if len(signIn) > 0 {
		started, err := p.startSignIn(ctx, d, name, c.Namespace, pid, signIn[0])
		if err != nil {
			return fail("sign-in", err.Error())
		}
		out.SignInURL, out.SignInCookie = started.SignInURL, started.SignInCookie
	}
	return out, nil
}

// approveReady approves the tools of each read-pending server whose
// credential is present, and returns the servers to check, the OAuth
// servers still waiting for a sign-in, and why a tools approval failed.
func (p connectorPages) approveReady(ctx context.Context, d *approverdevice.Device, name, ns string, grants []agentadmin.IntegrationGrant) (checked, signIn []string, why string) {
	for _, g := range grants {
		if g.ReadPending {
			if oauth, present := p.serverCredential(ctx, ns, g.Project, g.Name); oauth && !present {
				signIn = append(signIn, g.Name)
				continue
			}
			if why := p.approveTools(ctx, d, name, ns, g.Project, g.Name); why != "" {
				return nil, nil, why
			}
		}
		checked = append(checked, g.Name)
	}
	return checked, signIn, ""
}

// serverCredential reports whether a loaded server signs in with OAuth and
// whether its credential (the token, or the stored secret) is present.
func (p connectorPages) serverCredential(ctx context.Context, ns, projectID, server string) (oauth, present bool) {
	pr := p.s.c.Registry.GetProject(projectID)
	if pr == nil {
		return false, false
	}
	for _, srv := range pr.MCP.Servers {
		if srv.Name != server {
			continue
		}
		if srv.Auth.EffectiveMode() == mcpauth.ModeOAuth {
			return true, p.s.credentialPresent(ctx, ns, projectID, agentadmin.OAuthCredentialName(server), agentadmin.CredentialOAuth)
		}
		name, ok := strings.CutPrefix(srv.Auth.ValueFrom, "secret://"+ns+"/")
		return false, !ok || p.s.credentialPresent(ctx, ns, projectID, name, agentadmin.CredentialSecret)
	}
	return false, false
}

// startSignIn files the server's OAuth credential_slot (no push), marked
// with the connector so its callback leaves the tools to the second
// confirm, and starts the device sign-in for it (design §19.14.10). A slot
// already pending for the credential is started instead.
func (p connectorPages) startSignIn(ctx context.Context, d *approverdevice.Device, name, ns, projectID, server string) (approverdevice.ConnectorOutcome, error) {
	s := p.s
	cred := agentadmin.OAuthCredentialName(server)
	slug := strings.TrimPrefix(projectID, ns+agentns.Separator)
	raw, _ := json.Marshal(agentadmin.RequestCredentialInput{Project: slug, Name: cred, Purpose: "Sign in for the " + name + " connector", Kind: agentadmin.CredentialOAuth})
	var row persistence.AgentApprovalRequestRow
	s.mu.Lock()
	st, err := s.loadState(ctx, ns, agentns.ID(ns, "home"))
	if err != nil {
		s.mu.Unlock()
		return approverdevice.ConnectorOutcome{}, err
	}
	ch, err := s.renderer.Render(st, agentadmin.VerbRequestCredential, raw)
	switch {
	case err != nil:
	case ch.Slot != nil:
		row, err = requestRowOf(persistence.ApprovalKindCredentialSlot, approvalPayload{Change: ch.Rendered, Locks: ch.Locks, Slot: ch.Slot, Connector: name}, ch)
		if err == nil {
			err = s.requests.CreateRequest(ctx, row)
		}
	default:
		// Already requested: the pending slot is the one to sign in with.
		pending, _ := s.credentialRequestStates(ctx, ns)
		id := pending[projectID+"/"+cred]
		var got *persistence.AgentApprovalRequestRow
		if id != "" {
			got, err = s.requests.GetRequest(ctx, id)
		}
		if got == nil && err == nil {
			err = errors.New("the sign-in for " + server + " could not be requested")
		}
		if got != nil {
			row = *got
		}
	}
	s.mu.Unlock()
	if err != nil {
		return approverdevice.ConnectorOutcome{}, err
	}
	authURL, cookie, err := agentOAuthConnect{s: s}.Start(ctx, d, row, row.RenderedSHA256)
	if err != nil {
		return approverdevice.ConnectorOutcome{}, errors.New("the sign-in for " + server + " could not start")
	}
	return approverdevice.ConnectorOutcome{SignInURL: authURL, SignInCookie: cookie}, nil
}

// installServers are the servers of the device's install of connector name
// in the choice's project and account, or nil when the device has none.
func (p connectorPages) installServers(ctx context.Context, name string, c approverdevice.ConnectorChoice) (string, []string) {
	s := p.s
	rec := s.renderer.Catalogue().Get(name)
	if !rec.IsConnector() || !agentns.Valid(c.Namespace) {
		return "", nil
	}
	pid := agentns.ID(c.Namespace, agentadmin.ProjectSlug(c.Namespace, c.Project))
	label := agentns.AccountOf(c.Account)
	rows, err := s.grants.ListConnectorInstalls(ctx, c.Namespace)
	if err != nil {
		return "", nil
	}
	found := false
	for _, r := range rows {
		found = found || (r.Connector == name && r.ProjectID == pid && agentns.AccountOf(r.AccountLabel) == label)
	}
	if !found {
		return "", nil
	}
	var out []string
	for _, n := range rec.Needs {
		server := n.Name
		if label != agentns.DefaultAccount {
			server += "-" + label
		}
		out = append(out, server)
	}
	return pid, out
}

// pendingOf sorts an install's read-pending servers into those that need a
// sign-in and those ready for the tools approval, with the rendered tools
// approvals of the ready ones. The caller holds s.mu.
func (p connectorPages) pendingOf(ctx context.Context, name string, c approverdevice.ConnectorChoice) (string, []string, []agentadmin.Change, error) {
	s := p.s
	pid, servers := p.installServers(ctx, name, c)
	if pid == "" {
		return "", nil, nil, nil
	}
	var signIn []string
	var ready []agentadmin.Change
	var st *agentadmin.State
	for _, server := range servers {
		a, err := s.grants.GetIntegration(ctx, pid, server)
		if err != nil || a.RemovedAt != nil || !a.ReadPending {
			continue
		}
		oauth, present := p.serverCredential(ctx, c.Namespace, pid, server)
		if !oauth {
			continue // a secret server is the agent path's to finish (§19.14.8 Recovery)
		}
		if !present {
			signIn = append(signIn, server)
			continue
		}
		if st == nil {
			if st, err = s.loadState(ctx, c.Namespace, agentns.ID(c.Namespace, "home")); err != nil {
				return "", nil, nil, err
			}
		}
		var tools []string
		if pr, ok := st.Projects[pid]; ok {
			for _, srv := range pr.Servers {
				if srv.Name == server {
					tools = srv.Tools
				}
			}
		}
		raw, _ := json.Marshal(agentadmin.ApproveServerToolsInput{Project: pid, Server: server, Tools: tools})
		ch, err := s.renderer.Render(st, agentadmin.VerbApproveServerTools, raw)
		if err != nil {
			return "", nil, nil, err
		}
		if ch.Class == agentadmin.Widening {
			ready = append(ready, ch)
		}
	}
	return pid, signIn, ready, nil
}

// shownOfAll binds the second confirm to every tools approval it shows.
func shownOfAll(chs []agentadmin.Change) string {
	h := sha256.New()
	for _, ch := range chs {
		h.Write([]byte(shownOf(ch)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Pending is what the device's install of name still waits for.
func (p connectorPages) Pending(ctx context.Context, name string, c approverdevice.ConnectorChoice) (approverdevice.ConnectorPending, error) {
	p.s.mu.Lock()
	_, signIn, ready, err := p.pendingOf(ctx, name, c)
	p.s.mu.Unlock()
	if err != nil {
		return approverdevice.ConnectorPending{}, err
	}
	out := approverdevice.ConnectorPending{SignIn: signIn}
	var sentences []string
	for _, ch := range ready {
		out.Ready = append(out.Ready, ch.Grant.Integrations[0].Name)
		sentences = append(sentences, ch.Sentence)
	}
	if len(ready) > 0 {
		out.Sentence, out.Shown = strings.Join(sentences, " "), shownOfAll(ready)
	}
	return out, nil
}

// SignIn starts the sign-in for one server the install waits on.
func (p connectorPages) SignIn(ctx context.Context, d *approverdevice.Device, name string, c approverdevice.ConnectorChoice, server string) (approverdevice.ConnectorOutcome, error) {
	if d == nil {
		return approverdevice.ConnectorOutcome{}, approverdevice.ErrNoDevice
	}
	p.s.mu.Lock()
	pid, signIn, _, err := p.pendingOf(ctx, name, c)
	p.s.mu.Unlock()
	if err != nil {
		return approverdevice.ConnectorOutcome{}, err
	}
	for _, s := range signIn {
		if s == server {
			return p.startSignIn(ctx, d, name, c.Namespace, pid, server)
		}
	}
	return approverdevice.ConnectorOutcome{}, errors.New("this connector does not wait for a sign-in to " + server)
}

// Finish is the second confirm: the tools approvals the page showed, then
// the connection check (design §19.14.10).
func (p connectorPages) Finish(ctx context.Context, d *approverdevice.Device, name string, c approverdevice.ConnectorChoice, shown string) (approverdevice.ConnectorOutcome, error) {
	if d == nil {
		return approverdevice.ConnectorOutcome{}, approverdevice.ErrNoDevice
	}
	p.s.mu.Lock()
	pid, _, ready, err := p.pendingOf(ctx, name, c)
	p.s.mu.Unlock()
	switch {
	case err != nil:
		return approverdevice.ConnectorOutcome{}, err
	case len(ready) == 0:
		return approverdevice.ConnectorOutcome{Refused: "nothing is waiting for its tools approval: sign in first"}, nil
	case shown != shownOfAll(ready):
		return approverdevice.ConnectorOutcome{Refused: "what it would approve changed since this page was shown; review it again"}, nil
	}
	out := approverdevice.ConnectorOutcome{Installed: true}
	for _, ch := range ready {
		server := ch.Grant.Integrations[0].Name
		if why := p.approveTools(ctx, d, name, c.Namespace, pid, server); why != "" {
			out.Failed, out.FailedWhy = connectorStepTools, why
			return out, nil
		}
		out.Checks = append(out.Checks, approverdevice.ConnectorCheck{Server: server, Code: p.check(ctx, d, pid, server)})
	}
	return out, nil
}

// filedInstall is the install filed for the device to decide.
type filedInstall struct {
	ch  agentadmin.Change
	row persistence.AgentApprovalRequestRow
}

// fileInstall renders the install, checks it is the one shown and every
// credential before anything is filed, then files the proposal and its
// request row without a push. A refusal files nothing.
func (p connectorPages) fileInstall(ctx context.Context, d *approverdevice.Device, name string, c approverdevice.ConnectorChoice, shown string, creds map[string][]byte) (filedInstall, approverdevice.ConnectorOutcome, error) {
	s := p.s
	refuse := func(why, sentence string) approverdevice.ConnectorOutcome {
		return approverdevice.ConnectorOutcome{Refused: why, Sentence: sentence}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := p.renderConnector(ctx, name, c)
	switch {
	case err != nil:
		return filedInstall{}, approverdevice.ConnectorOutcome{}, err
	case r.missing:
		return filedInstall{}, refuse("create the project first", ""), nil
	case r.refused != "":
		return filedInstall{}, refuse(r.refused, ""), nil
	}
	ch := r.ch
	if shown != shownOf(ch) {
		return filedInstall{}, refuse("what it would install changed since this page was shown; review it again", ch.Sentence), nil
	}
	// Every credential before step d (review-20261013-f1f5 N5): typed and
	// acceptable, or left blank and already stored.
	for _, f := range ch.Credentials {
		if f.Kind == agentadmin.CredentialOAuth {
			continue // signed in after the install (design §19.14.10)
		}
		v := creds[f.Name]
		switch {
		case len(v) == 0 && !s.credentialPresent(ctx, c.Namespace, f.Project, f.Name, f.Kind):
			return filedInstall{}, refuse("enter "+f.Name, ""), nil
		case len(v) > 0 && !credentialValueAcceptable(v):
			return filedInstall{}, refuse(f.Name+" is empty, too long, or only spaces", ""), nil
		}
	}
	st, err := s.loadState(ctx, c.Namespace, agentns.ID(c.Namespace, "home"))
	if err != nil {
		return filedInstall{}, approverdevice.ConnectorOutcome{}, err
	}
	coverID, err := s.ensureCover(ctx, st)
	if err != nil {
		return filedInstall{}, approverdevice.ConnectorOutcome{}, err
	}
	if why, err := s.spendingBlocked(st, ch, coverID); err != nil {
		return filedInstall{}, approverdevice.ConnectorOutcome{}, err
	} else if why != "" {
		return filedInstall{}, refuse(why, ""), nil
	}
	if len(ch.Ops) > 12 {
		return filedInstall{}, refuse("the change touches more than 12 files", ""), nil
	}
	prop, err := s.fileProposalBy(ctx, connectorProposer(name), connectorActorKind, d.ID, ch)
	if err != nil {
		return filedInstall{}, approverdevice.ConnectorOutcome{}, err
	}
	// No Credentials in the payload (design §19.14.8 item 5d; review 99c9
	// F2): applyApproved then files no credential request and schedules no
	// background tools listing.
	row, err := requestRowOf(persistence.ApprovalKindWideningChange,
		approvalPayload{ProposalID: prop.ID, Change: ch.Rendered, Grant: ch.Grant, Narrow: ch.Narrow, Locks: ch.Locks}, ch)
	if err != nil {
		return filedInstall{}, approverdevice.ConnectorOutcome{}, err
	}
	if err := s.requests.CreateRequest(ctx, row); err != nil {
		return filedInstall{}, approverdevice.ConnectorOutcome{}, err
	}
	return filedInstall{ch: ch, row: row}, approverdevice.ConnectorOutcome{}, nil
}

// storeCredential files the credential's slot without a push and decides
// it with the value: the agent path's decide-then-store, without its
// detached tools listing (design §19.14.8 item 5e). It says why it failed.
func (p connectorPages) storeCredential(ctx context.Context, d *approverdevice.Device, ns string, f agentadmin.CredentialFollowUp, value []byte) string {
	s := p.s
	slug := strings.TrimPrefix(f.Project, ns+agentns.Separator)
	raw, _ := json.Marshal(agentadmin.RequestCredentialInput{Project: slug, Name: f.Name, Purpose: f.Purpose, Kind: f.Kind})
	s.mu.Lock()
	st, err := s.loadState(ctx, ns, agentns.ID(ns, "home"))
	if err != nil {
		s.mu.Unlock()
		return "the setup could not be read; try again"
	}
	ch, err := s.renderer.Render(st, agentadmin.VerbRequestCredential, raw)
	if err != nil || ch.Slot == nil {
		s.mu.Unlock()
		why := "a request for " + f.Name + " is already waiting on your Approvals page; enter it there"
		if err == nil && ch.Class == agentadmin.Refused {
			why = ch.Reason
		}
		return why
	}
	row, err := requestRowOf(persistence.ApprovalKindCredentialSlot, approvalPayload{Change: ch.Rendered, Locks: ch.Locks, Slot: ch.Slot}, ch)
	if err == nil {
		err = s.requests.CreateRequest(ctx, row)
	}
	s.mu.Unlock()
	if err != nil {
		return "the credential request could not be filed; try again"
	}
	if _, err := s.decideAndStoreCredential(ctx, d, row, row.RenderedSHA256, value); err != nil {
		return f.Name + " was not stored"
	}
	return ""
}

// approveTools files approve_server_tools for one read-pending server with
// exactly the tools the install declared (the recipe's read tools) and
// decides it as the device. It says why it failed.
func (p connectorPages) approveTools(ctx context.Context, d *approverdevice.Device, name, ns, projectID, server string) string {
	s := p.s
	s.mu.Lock()
	st, err := s.loadState(ctx, ns, agentns.ID(ns, "home"))
	if err != nil {
		s.mu.Unlock()
		return "the setup could not be read; try again"
	}
	var tools []string
	if pr, ok := st.Projects[projectID]; ok {
		for _, srv := range pr.Servers {
			if srv.Name == server {
				tools = srv.Tools
			}
		}
	}
	raw, _ := json.Marshal(agentadmin.ApproveServerToolsInput{Project: projectID, Server: server, Tools: tools})
	ch, err := s.renderer.Render(st, agentadmin.VerbApproveServerTools, raw)
	if err != nil || ch.Class != agentadmin.Widening {
		s.mu.Unlock()
		if err == nil && ch.Reason != "" {
			return ch.Reason
		}
		return "the tools of " + server + " could not be approved"
	}
	prop, err := s.fileProposalBy(ctx, connectorProposer(name), connectorActorKind, d.ID, ch)
	var row persistence.AgentApprovalRequestRow
	if err == nil {
		row, err = requestRowOf(persistence.ApprovalKindWideningChange,
			approvalPayload{ProposalID: prop.ID, Change: ch.Rendered, Grant: ch.Grant, Narrow: ch.Narrow, Locks: ch.Locks}, ch)
	}
	if err == nil {
		err = s.requests.CreateRequest(ctx, row)
	}
	s.mu.Unlock()
	if err != nil {
		return "the tools approval could not be filed; try again"
	}
	if err := s.devices.Decide(ctx, d, row.ID, row.RenderedSHA256, true); err != nil {
		return "the tools approval could not be applied: " + err.Error()
	}
	return ""
}

// check probes one server as test_connection does, rate-limited per device,
// and records the audit row list_my_setup reads.
func (p connectorPages) check(ctx context.Context, d *approverdevice.Device, projectID, server string) string {
	s := p.s
	if why := s.limiter.allow("device:"+d.ID, projectID, server, s.now()); why != "" {
		return "not checked: " + why
	}
	pr := s.c.Registry.GetProject(projectID)
	if pr == nil {
		return string(agentadmin.ConnNotApproved)
	}
	var srv *registry.MCPServerConfig
	for i := range pr.MCP.Servers {
		if pr.MCP.Servers[i].Name == server {
			srv = &pr.MCP.Servers[i]
		}
	}
	if srv == nil {
		return string(agentadmin.ConnNotApproved)
	}
	pctx, cancel := context.WithTimeout(ctx, s.testConnTimeoutOrDefault())
	defer cancel()
	code, class := s.probeMCP(pctx, pr, *srv, nil)
	s.auditTestConn(ctx, projectID, server, agentadmin.ConnectionResult{Code: code, HTTPStatusClass: class, CheckedAt: s.now().UTC()})
	return string(code)
}

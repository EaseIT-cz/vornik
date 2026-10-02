package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/imagemanifest"
	"vornik.io/vornik/internal/persistence"
)

// The agent admin verbs (agent-administered Vornik design §6–§7; plan P3.5,
// P3.6): render a verb, file it through the proposal ledger, and apply it now
// (inert) or ask an approver device (widening).

// AgentAdminInertActor approves inert agent changes in the ledger. A widening
// change is approved by "device:<id>", so the agent never approves its own.
const AgentAdminInertActor = "system:agent-admin-inert"

// AgentAdminResult is a mutating verb's outcome (§6).
type AgentAdminResult = agentadmin.Result

// Effects.
const (
	EffectApplied  = agentadmin.EffectApplied
	EffectAwaiting = agentadmin.EffectAwaiting
	EffectRefused  = agentadmin.EffectRefused
)

type agentAdminService struct {
	c         *Container
	renderer  *agentadmin.Renderer
	devices   *approverdevice.Service
	grants    persistence.AgentGrantRepository
	requests  persistence.ApproverDeviceRepository
	proposals persistence.ProposalRepository
	engine    *controlplane.ApplyEngine
	// configDir holds config.yaml (the apply engine's root); configsDir the
	// registry tree. Ops are rendered relative to configsDir.
	configDir, configsDir string
	// mu serialises verb calls: state read, render and filing are one unit.
	mu sync.Mutex
	// bg tracks detached work (a tools listing after a credential is
	// stored), so a caller can wait for it.
	bg sync.WaitGroup
}

// background runs fn detached and tracked.
func (s *agentAdminService) background(fn func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn()
	}()
}

// agentAdmin returns the verb service, or nil while a dependency is absent.
// It is built on first success and retried until then: a daemon started
// before make install-config-assets put the agent templates in place gets
// the verbs as soon as they are there, without a restart (regression, local
// deployment 2026-10-02: a once-only build cached the failure).
func (c *Container) agentAdmin() *agentAdminService {
	// The lock is held across the templates read on purpose: two callers
	// must not both build and register the device effects. While unbuilt,
	// callers serialise on it; once built it guards a nil check.
	c.agentAdminMu.Lock()
	defer c.agentAdminMu.Unlock()
	if c.agentAdminSvc != nil {
		return c.agentAdminSvc
	}
	if !c.agentAdminWireable() {
		return nil
	}
	configsDir := resolveRegistryConfigDir(c.ConfigPath)
	// The templates first: the cheap check that fails until the assets are
	// installed, before anything else is built.
	r, err := agentadmin.NewRenderer(os.DirFS(filepath.Join(configsDir, "agent-templates")))
	if err != nil {
		if time.Since(c.agentAdminWarned) > time.Minute {
			c.agentAdminWarned = time.Now()
			c.Logger.Warn().Err(err).Msg("agent admin verbs unavailable until the agent templates are installed (make install-config-assets); retrying")
		}
		return nil
	}
	devices := c.approverDeviceService()
	engine := c.newProposalApplier()
	if devices == nil || engine == nil {
		// Wireable but not buildable: say which, or companion-admin stays
		// false with no trail (review 20261002-a45d F1).
		if time.Since(c.agentAdminWarned) > time.Minute {
			c.agentAdminWarned = time.Now()
			c.Logger.Warn().Bool("approver_devices", devices != nil).Bool("proposal_engine", engine != nil).
				Msg("agent admin verbs unavailable: a dependency is not wired; retrying")
		}
		return nil
	}
	s := &agentAdminService{c: c, renderer: r, devices: devices, grants: c.repos.AgentGrants,
		requests: c.repos.ApproverDevices, proposals: c.repos.Proposals, engine: engine,
		configDir: filepath.Dir(c.ConfigPath), configsDir: configsDir}
	devices.RegisterEffect(persistence.ApprovalKindWideningChange, s.applyApproved)
	devices.RegisterOnReject(persistence.ApprovalKindWideningChange, s.rejectApproved)
	// A credential slot's effect stores what its decision carried: the
	// typed value or the sign-in (plan P4.2/P4.4, decide then store).
	devices.RegisterEffect(persistence.ApprovalKindCredentialSlot, s.slotEffect)
	devices.RegisterValueEntry(persistence.ApprovalKindCredentialSlot, s.enterCredential)
	devices.RegisterConnect(persistence.ApprovalKindCredentialSlot, agentOAuthConnect{s: s})
	// A proposed write is approved on the phone, one request per action
	// (plan P4.8).
	devices.RegisterEffect(persistence.ApprovalKindBrokerAction, s.actionEffect)
	devices.RegisterOnReject(persistence.ApprovalKindBrokerAction, s.actionRejected)
	c.agentAdminSvc = s
	return c.agentAdminSvc
}

// agentAdminWireable reports whether the structural dependencies of the
// agent admin service exist; only the templates may arrive later.
func (c *Container) agentAdminWireable() bool {
	return c.repos != nil && c.repos.AgentGrants != nil && c.repos.Proposals != nil && c.repos.ApproverDevices != nil && c.Registry != nil &&
		resolveRegistryConfigDir(c.ConfigPath) != ""
}

// agentAdminProxy is what the API server holds: it reaches the service on
// every call, so the verbs appear once their templates do.
type agentAdminProxy struct{ c *Container }

func (p agentAdminProxy) Do(ctx context.Context, key *persistence.APIKey, verb string, input json.RawMessage) (agentadmin.Result, error) {
	s := p.c.agentAdmin()
	if s == nil {
		return agentadmin.Result{}, agentadmin.ErrUnavailable
	}
	return s.Do(ctx, key, verb, input)
}

func (p agentAdminProxy) ListSetupJSON(ctx context.Context, key *persistence.APIKey) (any, error) {
	s := p.c.agentAdmin()
	if s == nil {
		return nil, agentadmin.ErrUnavailable
	}
	return s.ListSetupJSON(ctx, key)
}

func (p agentAdminProxy) DescribeJSON(ctx context.Context, key *persistence.APIKey) (any, error) {
	s := p.c.agentAdmin()
	if s == nil {
		return nil, agentadmin.ErrUnavailable
	}
	return s.DescribeJSON(ctx, key)
}

func (p agentAdminProxy) EnsureHome(ctx context.Context, ns, clientKind string) (string, error) {
	s := p.c.agentAdmin()
	if s == nil {
		return "", agentadmin.ErrUnavailable
	}
	return s.EnsureHome(ctx, ns, clientKind)
}

// Do runs one mutating verb for an agent admin key.
func (s *agentAdminService) Do(ctx context.Context, key *persistence.APIKey, verb string, input json.RawMessage) (AgentAdminResult, error) {
	if key == nil || !key.AgentAdmin || !agentns.Valid(key.AgentNamespace) {
		return AgentAdminResult{}, fmt.Errorf("not an agent admin key")
	}
	ns := key.AgentNamespace
	if verb == agentadmin.VerbApproveServerTools {
		// Daemon-internal (plan P4.3): the daemon files it after listing a
		// connected server's tools; no key may.
		return AgentAdminResult{Effect: EffectRefused, Reason: verb + " refused: not a verb you can call"}, nil
	}
	// Listing a new unauthenticated server is network I/O: before the lock.
	var advertisedURL string
	var advertised []string
	if verb == agentadmin.VerbAddMCPServer {
		advertisedURL, advertised = s.advertiseUnauthenticated(ctx, ns, input)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadState(ctx, ns, key.ProjectID)
	if err != nil {
		return AgentAdminResult{}, err
	}
	if advertisedURL != "" {
		st.Advertised[advertisedURL] = advertised
	}
	ch, err := s.renderer.Render(st, verb, input)
	if err != nil {
		return AgentAdminResult{}, err
	}
	if ch.Class == agentadmin.Refused {
		return AgentAdminResult{Effect: EffectRefused, Reason: ch.Reason}, nil
	}
	if ch.Slot != nil {
		return s.fileRequest(ctx, persistence.ApprovalKindCredentialSlot, "",
			approvalPayload{Change: ch.Rendered, Locks: ch.Locks, Slot: ch.Slot}, ch)
	}
	if len(ch.Ops) > 12 {
		return AgentAdminResult{Effect: EffectRefused, Reason: verb + " refused: the change touches more than 12 files; remove the project's workflows first"}, nil
	}
	p, err := s.fileProposal(ctx, "companion:"+key.ClientKind, key.ID, ch)
	if err != nil {
		return AgentAdminResult{}, err
	}
	if ch.Class == agentadmin.Inert {
		return s.applyInert(ctx, p, ch)
	}
	return s.requestApproval(ctx, p, ch)
}

// opRel maps a configs-relative path to the apply engine's root.
func (s *agentAdminService) opRel(path string) string {
	rel, err := filepath.Rel(s.configDir, s.configsDir)
	if err != nil || rel == "." {
		return path
	}
	return filepath.ToSlash(filepath.Join(rel, path))
}

func (s *agentAdminService) fileProposal(ctx context.Context, actorKind, credentialID string, ch agentadmin.Change) (*persistence.ControlPlaneProposal, error) {
	ops := make([]map[string]string, 0, len(ch.Ops))
	for _, op := range ch.Ops {
		ops = append(ops, map[string]string{"op": op.Op, "path": s.opRel(op.Path), "content": op.Content})
	}
	readSet := map[string]string{}
	for path, h := range ch.ReadSet {
		readSet[s.opRel(path)] = h
	}
	rawOps, err := json.Marshal(ops)
	if err != nil {
		return nil, err
	}
	evidence, err := json.Marshal(map[string]any{"read_set": readSet})
	if err != nil {
		return nil, err
	}
	project := ""
	for _, l := range ch.Locks {
		if id, ok := strings.CutPrefix(l, "project:"); ok {
			project = id
			break
		}
	}
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: project,
		Kind: persistence.ProposalKindScaffold, BlastRadius: persistence.ProposalScopeProject,
		Title: truncate(ch.Sentence, 200), ApplyOps: string(rawOps), Evidence: string(evidence),
		Status: persistence.ProposalStatusDraft, ProposedBy: "agent:" + ch.Namespace,
		Entrypoint: "agent", ActorKind: actorKind, ActorCredentialID: credentialID,
	}
	if err := s.proposals.Create(ctx, p); err != nil {
		return nil, fmt.Errorf("file the change: %w", err)
	}
	return p, nil
}

// applyInert approves an inert change as the system actor and applies it.
func (s *agentAdminService) applyInert(ctx context.Context, p *persistence.ControlPlaneProposal, ch agentadmin.Change) (AgentAdminResult, error) {
	if err := s.proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, AgentAdminInertActor); err != nil {
		return AgentAdminResult{}, err
	}
	if why := s.applyAndVerify(ctx, p.ID, AgentAdminInertActor, ch); why != "" {
		return AgentAdminResult{ChangeID: p.ID, Effect: EffectRefused, Reason: ch.Verb + " refused: " + why}, nil
	}
	if err := s.recordNarrowing(ctx, ch.Narrow); err != nil {
		return AgentAdminResult{}, err
	}
	return AgentAdminResult{ChangeID: p.ID, Effect: EffectApplied, Sentence: ch.Sentence}, nil
}

// applyAndVerify applies an APPROVED proposal and confirms the result loaded.
// The reload strips an invalid project with a warning, so "applied" alone is
// not proof (plan P3 global rules). A miss rolls back. It returns why the
// change did not take, or "".
func (s *agentAdminService) applyAndVerify(ctx context.Context, id, actor string, ch agentadmin.Change) string {
	if err := s.engine.Apply(ctx, id, actor, false); err != nil {
		_ = s.proposals.SetStatus(ctx, id, persistence.ProposalStatusRejected, actor)
		switch {
		case errors.Is(err, controlplane.ErrBusy):
			return "a task of this project is running; try again when it finishes"
		case errors.Is(err, controlplane.ErrStaleBase):
			return "a file this change was prepared against has changed; prepare it again"
		case errors.Is(err, controlplane.ErrWriterFenced):
			return "this Vornik node is not the one that changes configuration right now; try again shortly"
		}
		return "the change could not be applied: " + err.Error()
	}
	if missing := s.verifyLoaded(ch); missing != "" {
		if rerr := s.engine.Rollback(ctx, id); rerr != nil {
			s.c.Logger.Error().Err(rerr).Str("proposal_id", id).Msg("agent admin: rollback after a failed verify failed")
		}
		return "Vornik could not load " + missing + "; the change was rolled back"
	}
	return ""
}

// verifyLoaded checks every op's entity against the live registry.
func (s *agentAdminService) verifyLoaded(ch agentadmin.Change) string {
	reg := s.c.Registry
	for _, op := range ch.Ops {
		dir, file := filepath.Split(op.Path)
		id := strings.TrimSuffix(strings.TrimSuffix(file, ".yaml"), ".md")
		var present bool
		switch dir {
		case "projects/":
			present = reg.GetProject(id) != nil
		case "swarms/":
			present = reg.GetSwarm(id) != nil
		case "workflows/":
			present = reg.GetWorkflow(id) != nil
		default:
			continue
		}
		if present == (op.Op == agentadmin.OpDelete) {
			return id
		}
	}
	return ""
}

// recordNarrowing withdraws approvals a removal makes moot. Grants are never
// written here: only the widening_change effect writes them.
func (s *agentAdminService) recordNarrowing(ctx context.Context, n agentadmin.Narrowing) error {
	now := time.Now().UTC()
	for _, ref := range n.RemovedIntegrations {
		if err := s.grants.MarkIntegrationRemoved(ctx, ref.Project, ref.Name, now); err != nil {
			return err
		}
	}
	for _, wf := range n.RemovedWorkflows {
		if err := s.grants.DeleteWorkflowReach(ctx, wf); err != nil {
			return err
		}
	}
	return nil
}

// approvalPayload is what an approval request carries: the proposal it
// approves, what approving records, and what it holds while pending.
type approvalPayload struct {
	ProposalID string               `json:"proposal_id"`
	Change     json.RawMessage      `json:"change"`
	Grant      agentadmin.Grant     `json:"grant"`
	Narrow     agentadmin.Narrowing `json:"narrowing"`
	Locks      []string             `json:"locks"`
	Ops        []agentadmin.FileOp  `json:"-"`
	// Slot is a credential_slot request's typed content (plan P4.1).
	Slot *agentadmin.CredentialSlot `json:"slot,omitempty"`
}

func (s *agentAdminService) requestApproval(ctx context.Context, p *persistence.ControlPlaneProposal, ch agentadmin.Change) (AgentAdminResult, error) {
	return s.fileRequest(ctx, persistence.ApprovalKindWideningChange, p.ID,
		approvalPayload{ProposalID: p.ID, Change: ch.Rendered, Grant: ch.Grant, Narrow: ch.Narrow, Locks: ch.Locks}, ch)
}

// fileRequest files an approval request of kind for the device; changeID is
// the proposal behind it ("" for a credential slot, which has none).
func (s *agentAdminService) fileRequest(ctx context.Context, kind, changeID string, payload approvalPayload, ch agentadmin.Change) (AgentAdminResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return AgentAdminResult{}, err
	}
	canon, err := approval.Canonical(raw)
	if err != nil {
		return AgentAdminResult{}, err
	}
	sum, err := approval.CanonicalSHA256(canon)
	if err != nil {
		return AgentAdminResult{}, err
	}
	now := time.Now().UTC()
	id := persistence.GenerateID("apr")
	row := persistence.AgentApprovalRequestRow{
		ID: id, Namespace: ch.Namespace, Kind: kind,
		Sentence: ch.Sentence, Rendered: canon, RenderedSHA256: sum, Status: persistence.ApprovalPending,
		CreatedAt: now, ExpiresAt: now.Add(approverdevice.RequestTTL),
	}
	if err := s.devices.FileRequest(ctx, row); err != nil {
		return AgentAdminResult{}, err
	}
	url := strings.TrimRight(s.c.Config.PublicOrigin(), "/") + "/ui/approve/" + id
	return AgentAdminResult{ChangeID: changeID, Effect: EffectAwaiting, ApprovalURL: url, Sentence: ch.Sentence}, nil
}

// applyApproved is the widening_change effect. It is idempotent: an already
// applied proposal skips to recording the grant, which is an upsert.
func (s *agentAdminService) applyApproved(ctx context.Context, r persistence.AgentApprovalRequestRow) error {
	var pl approvalPayload
	if err := json.Unmarshal(r.Rendered, &pl); err != nil || pl.ProposalID == "" {
		return fmt.Errorf("%w: the request does not name its change", approverdevice.ErrPermanent)
	}
	p, err := s.proposals.GetByID(ctx, pl.ProposalID)
	if err != nil {
		return err
	}
	actor := "device:" + r.DecidedByDevice
	switch p.Status {
	case persistence.ProposalStatusApplied:
	case persistence.ProposalStatusDraft, persistence.ProposalStatusApproved:
		if p.Status == persistence.ProposalStatusDraft {
			if err := s.proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, actor); err != nil {
				return err
			}
		}
		ch, err := s.changeOf(p)
		if err != nil {
			return fmt.Errorf("%w: %v", approverdevice.ErrPermanent, err)
		}
		s.mu.Lock()
		why := s.applyAndVerify(ctx, p.ID, actor, ch)
		s.mu.Unlock()
		if why != "" {
			if strings.HasPrefix(why, "a task of this project is running") {
				return errors.New(why) // transient: the tick retries
			}
			return fmt.Errorf("%w: %s", approverdevice.ErrPermanent, why)
		}
	default:
		return fmt.Errorf("%w: the change is %s", approverdevice.ErrPermanent, strings.ToLower(p.Status))
	}
	return s.recordGrant(ctx, r, pl)
}

// changeOf rebuilds the ops of a filed proposal, for verify.
func (s *agentAdminService) changeOf(p *persistence.ControlPlaneProposal) (agentadmin.Change, error) {
	var ops []agentadmin.FileOp
	if err := json.Unmarshal([]byte(p.ApplyOps), &ops); err != nil {
		return agentadmin.Change{}, err
	}
	rel, _ := filepath.Rel(s.configDir, s.configsDir)
	for i := range ops {
		if rel != "" && rel != "." {
			ops[i].Path = strings.TrimPrefix(ops[i].Path, filepath.ToSlash(rel)+"/")
		}
	}
	return agentadmin.Change{Ops: ops}, nil
}

func (s *agentAdminService) recordGrant(ctx context.Context, r persistence.AgentApprovalRequestRow, pl approvalPayload) error {
	at := time.Now().UTC()
	if r.DecidedAt != nil {
		at = *r.DecidedAt
	}
	for _, g := range pl.Grant.Integrations {
		if err := s.grants.UpsertIntegration(ctx, persistence.AgentIntegrationApproval{
			Namespace: r.Namespace, ProjectID: g.Project, Integration: g.Name, Kind: g.Kind, URL: g.URL,
			ReadTools: g.Read, WriteTools: g.Write, ReadPending: g.ReadPending,
			ApprovedByDevice: r.DecidedByDevice, ApprovedAt: at,
		}); err != nil {
			return err
		}
	}
	for wf, hash := range pl.Grant.Workflows {
		if err := s.grants.UpsertWorkflowReach(ctx, persistence.AgentWorkflowApproval{
			Namespace: r.Namespace, WorkflowID: wf, ProjectID: agentadmin.ProjectOfWorkflow(wf), ReachHash: hash,
			ApprovedByDevice: r.DecidedByDevice, ApprovedAt: at,
		}); err != nil {
			return err
		}
	}
	if pl.Grant.CeilingUSD != nil {
		if err := s.grants.UpsertCeiling(ctx, persistence.AgentNamespaceBudget{
			Namespace: r.Namespace, CeilingUSD: *pl.Grant.CeilingUSD, ApprovedByDevice: r.DecidedByDevice, UpdatedAt: at,
		}); err != nil {
			return err
		}
	}
	return s.recordNarrowing(ctx, pl.Narrow)
}

// rejectApproved rejects the proposal behind a rejected request.
func (s *agentAdminService) rejectApproved(ctx context.Context, r persistence.AgentApprovalRequestRow) {
	var pl approvalPayload
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.ProposalID == "" {
		return
	}
	if err := s.proposals.SetStatus(ctx, pl.ProposalID, persistence.ProposalStatusRejected, "device:"+r.DecidedByDevice); err != nil {
		s.c.Logger.Warn().Err(err).Str("proposal_id", pl.ProposalID).Msg("agent admin: reject the proposal behind a rejected request")
	}
}

// loadState reads a namespace's state from the live registry, the config
// tree, the approval tables and the pending requests.
func (s *agentAdminService) loadState(ctx context.Context, ns, homeProject string) (*agentadmin.State, error) {
	cfg := s.c.Config.AgentAdmin
	st := &agentadmin.State{
		Namespace: ns, DefaultBudgetUSD: cfg.EffectiveDefaultProjectBudget(), CeilingUSD: cfg.EffectiveNamespaceBudget(),
		AgentImage: cfg.EffectiveAgentImage(imagemanifest.AgentImageTag),
		Projects:   map[string]*agentadmin.ProjectState{}, Workflows: map[string]*agentadmin.WorkflowState{},
		FileHashes: map[string]string{}, Locked: map[string]bool{},
		Approvals: map[string]map[string]agentadmin.IntegrationApproval{}, Advertised: map[string][]string{},
	}
	if mode, err := s.c.Config.Broker.WritesMode(); err == nil && mode == "on" {
		st.WritesOn = true // proposals need broker.writes (plan P4.8)
	}
	if b, err := s.grants.GetCeiling(ctx, ns); err == nil {
		st.CeilingUSD = b.CeilingUSD
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	reg := s.c.Registry
	for _, p := range reg.ListProjects() {
		if got, ok := agentns.FromID(p.ID); !ok || got != ns {
			continue
		}
		ps := agentadmin.ProjectStateFrom(p, reg.GetSwarm(p.SwarmID))
		ps.Home = p.ID == homeProject
		st.Projects[p.ID] = ps
	}
	reach, err := s.grants.ListWorkflowReach(ctx, ns)
	if err != nil {
		return nil, err
	}
	approved := map[string]string{}
	for _, a := range reach {
		approved[a.WorkflowID] = a.ReachHash
	}
	for _, wf := range reg.ListWorkflows() {
		if got, ok := agentns.FromID(wf.ID); !ok || got != ns {
			continue
		}
		st.Workflows[wf.ID] = &agentadmin.WorkflowState{ID: wf.ID, Project: agentadmin.ProjectOfWorkflow(wf.ID), ApprovedReach: approved[wf.ID], Loaded: wf}
	}
	if err := s.hashFiles(st, ns); err != nil {
		return nil, err
	}
	ints, err := s.grants.ListIntegrations(ctx, ns)
	if err != nil {
		return nil, err
	}
	for _, a := range ints {
		if st.Approvals[a.ProjectID] == nil {
			st.Approvals[a.ProjectID] = map[string]agentadmin.IntegrationApproval{}
		}
		st.Approvals[a.ProjectID][a.Integration] = agentadmin.IntegrationApproval{
			Kind: a.Kind, URL: a.URL, Read: a.ReadTools, Write: a.WriteTools, ReadPending: a.ReadPending, Removed: a.RemovedAt != nil,
		}
	}
	return st, s.lockPending(ctx, st, ns)
}

// hashFiles records the current bytes of every file of the namespace, loaded
// or not (a file the loader stripped still exists and still blocks a create).
func (s *agentAdminService) hashFiles(st *agentadmin.State, ns string) error {
	for _, dir := range []string{"projects", "swarms", "workflows"} {
		entries, err := os.ReadDir(filepath.Join(s.configsDir, dir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), ns+agentns.Separator) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.configsDir, dir, e.Name()))
			if err != nil {
				return err
			}
			st.FileHashes[dir+"/"+e.Name()] = agentadmin.HashContent(b)
		}
	}
	return nil
}

// lockPending marks what pending and approved-unapplied changes hold.
func (s *agentAdminService) lockPending(ctx context.Context, st *agentadmin.State, ns string) error {
	pending, err := s.requests.ListPending(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	unapplied, err := s.requests.ListApprovedUnapplied(ctx)
	if err != nil {
		return err
	}
	for _, r := range append(pending, unapplied...) {
		if r.Namespace != ns || (r.Kind != persistence.ApprovalKindWideningChange && r.Kind != persistence.ApprovalKindCredentialSlot) {
			continue
		}
		var pl approvalPayload
		if json.Unmarshal(r.Rendered, &pl) != nil {
			continue
		}
		for _, l := range pl.Locks {
			st.Locked[l] = true
		}
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

var _ = sort.Strings

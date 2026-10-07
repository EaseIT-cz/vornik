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

const agentAdminNotificationLimit = 100

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
	// now is the clock the cover request's 24-hour cooldown reads (§18.4
	// F10); a test moves it.
	now func() time.Time
	// toolGaps holds why a recipe server's tools were not filed for
	// approval (design §19.8 F6), keyed "<project>/<server>".
	gapsMu   sync.Mutex
	toolGaps map[string]string
	// notifications is a bounded, process-local advisory buffer of terminal
	// approval signals, keyed by request namespace.
	notificationsMu sync.Mutex
	notifications   map[string][]SetupNotification
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
		configDir: filepath.Dir(c.ConfigPath), configsDir: configsDir, now: time.Now}
	devices.RegisterEffect(persistence.ApprovalKindWideningChange, s.applyApproved)
	devices.RegisterOnReject(persistence.ApprovalKindWideningChange, s.rejectApproved)
	devices.RegisterTerminalObserver(persistence.ApprovalKindWideningChange, s.recordApprovalNotification)
	// A credential slot's effect stores what its decision carried: the
	// typed value or the sign-in (plan P4.2/P4.4, decide then store).
	devices.RegisterEffect(persistence.ApprovalKindCredentialSlot, s.slotEffect)
	devices.RegisterValueEntry(persistence.ApprovalKindCredentialSlot, s.enterCredential)
	devices.RegisterConnect(persistence.ApprovalKindCredentialSlot, agentOAuthConnect{s: s})
	devices.RegisterTerminalObserver(persistence.ApprovalKindCredentialSlot, s.recordApprovalNotification)
	// A proposed write is approved on the phone, one request per action
	// (plan P4.8).
	devices.RegisterEffect(persistence.ApprovalKindBrokerAction, s.actionEffect)
	devices.RegisterOnReject(persistence.ApprovalKindBrokerAction, s.actionRejected)
	// The approval page's plain summary and risk level (design §18.7).
	devices.RegisterDescriber(persistence.ApprovalKindWideningChange, describeRequest)
	devices.RegisterDescriber(persistence.ApprovalKindCredentialSlot, describeRequest)
	devices.RegisterDescriber(persistence.ApprovalKindBrokerAction, describeAction)
	c.agentAdminSvc = s
	return c.agentAdminSvc
}

func (s *agentAdminService) recordApprovalNotification(_ context.Context, r persistence.AgentApprovalRequestRow, status string) {
	if r.Namespace == "" {
		return
	}
	n := SetupNotification{ChangeID: r.ID, Kind: r.Kind, Status: status}
	s.notificationsMu.Lock()
	defer s.notificationsMu.Unlock()
	if s.notifications == nil {
		s.notifications = map[string][]SetupNotification{}
	}
	buf := s.notifications[r.Namespace]
	buf = append(buf, n)
	if len(buf) > agentAdminNotificationLimit {
		buf = append([]SetupNotification(nil), buf[len(buf)-agentAdminNotificationLimit:]...)
	}
	s.notifications[r.Namespace] = buf
}

func (s *agentAdminService) drainNotifications(ns string) []SetupNotification {
	s.notificationsMu.Lock()
	defer s.notificationsMu.Unlock()
	if len(s.notifications[ns]) == 0 {
		return nil
	}
	out := append([]SetupNotification(nil), s.notifications[ns]...)
	delete(s.notifications, ns)
	return out
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

func (p agentAdminProxy) ApprovedWorkflows(ctx context.Context, key *persistence.APIKey) ([]string, error) {
	s := p.c.agentAdmin()
	if s == nil {
		return nil, agentadmin.ErrUnavailable
	}
	return s.ApprovedWorkflows(ctx, key)
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
	var recipeTargets map[string][]string
	if verb == agentadmin.VerbInstallRecipe {
		recipeTargets = s.advertiseRecipeTargets(ctx, ns, input)
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
	for url, tools := range recipeTargets {
		st.Advertised[url] = tools
	}
	// A namespace above its ceiling owes a cover request (§18.4 F10): filed
	// here, from the state loaded under this lock and BEFORE the block
	// check, so the first refused verb names the request it filed.
	coverID, err := s.ensureCover(ctx, st)
	if err != nil {
		return AgentAdminResult{}, err
	}
	ch, err := s.renderer.Render(st, verb, input)
	if err != nil {
		return AgentAdminResult{}, err
	}
	if ch.Class == agentadmin.Refused {
		return AgentAdminResult{Effect: EffectRefused, Reason: ch.Reason}, nil
	}
	if why, err := s.spendingBlocked(st, ch, coverID); err != nil {
		return AgentAdminResult{}, err
	} else if why != "" {
		return AgentAdminResult{Effect: EffectRefused, Reason: why}, nil
	}
	if ch.Slot != nil {
		return s.fileRequest(ctx, persistence.ApprovalKindCredentialSlot, "",
			approvalPayload{Change: ch.Rendered, Locks: ch.Locks, Slot: ch.Slot}, ch)
	}
	if ch.Class == agentadmin.Inert && len(ch.Ops) == 0 {
		// Nothing differs (a recipe reinstalled as it is, design §19.11 4):
		// nothing is filed.
		return AgentAdminResult{Effect: EffectApplied, Sentence: ch.Sentence}, nil
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
	if err := s.recordToolRemovals(ctx, n.RemovedTools); err != nil {
		return err
	}
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
	// ReRenderOf names the request this one replaces: the daemon filed it
	// because that one's stated maximum was exceeded at apply (§18.4).
	ReRenderOf string `json:"re_render_of,omitempty"`
	// Credentials are the credential requests an approved recipe install
	// files next (design §19.9 F1).
	Credentials []agentadmin.CredentialFollowUp `json:"credentials,omitempty"`
}

func (s *agentAdminService) requestApproval(ctx context.Context, p *persistence.ControlPlaneProposal, ch agentadmin.Change) (AgentAdminResult, error) {
	return s.fileRequest(ctx, persistence.ApprovalKindWideningChange, p.ID,
		approvalPayload{ProposalID: p.ID, Change: ch.Rendered, Grant: ch.Grant, Narrow: ch.Narrow, Locks: ch.Locks, Credentials: ch.Credentials}, ch)
}

// fileRequest files an approval request of kind for the device; changeID is
// the proposal behind it ("" for a credential slot, which has none). Callers
// hold s.mu, and the device service's push is synchronous network I/O: it
// never calls back into this service, so holding the lock across it orders
// nothing new (review e1a4 F4), at the cost of holding it for one push.
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
// applied proposal skips to recording the grant, and a refused one finds the
// request it already filed in its place.
//
// The ceiling check, the apply and the grant are one unit under s.mu (design
// §18.4 item 2): the namespace sum with this change is checked against the
// most its sentence stated BEFORE anything applies, and the ceiling it
// writes is max(ceiling, sum), read and written under the same lock, so two
// approvals can neither both pass against a stale ceiling nor lower it.
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
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadStateExcluding(ctx, r.Namespace, agentns.ID(r.Namespace, "home"), r.ID)
	if err != nil {
		return err
	}
	if pl.Grant.Cover {
		// The daemon's cover request (§18.4 F10): the flag is read from the
		// row's canonical payload, the document the device approved.
		return s.applyCover(ctx, r, pl, p, actor, st)
	}
	// A model destination is re-resolved through the live router now
	// (design §18.6 item 2, round 3 F2): if it is not the one the device's
	// sentence named, nothing applies and nothing is recorded, so a row never
	// holds a destination nobody approved.
	if why := s.c.modelRouteMoved(pl.Grant.Models); why != "" {
		if p.Status == persistence.ProposalStatusDraft || p.Status == persistence.ProposalStatusApproved {
			if err := s.proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusRejected, actor); err != nil {
				return err
			}
		}
		return fmt.Errorf("%w: %s", approverdevice.ErrPermanent, why)
	}
	ceiling := st.CeilingUSD
	switch p.Status {
	case persistence.ProposalStatusApplied:
		// Applied before a crash: the sum already includes the change. The
		// ceiling it may write is still bounded by what the user approved.
		if pl.Grant.AddsUSD != nil || pl.Grant.CeilingUSD != nil {
			ceiling = max(ceiling, min(st.BudgetTotal(), agentadmin.MaxAllowedTotal(pl.Grant, st.CeilingUSD)))
		}
	case persistence.ProposalStatusDraft, persistence.ProposalStatusApproved:
		ch, err := s.changeOf(p)
		if err != nil {
			return fmt.Errorf("%w: %v", approverdevice.ErrPermanent, err)
		}
		after, err := st.BudgetTotalAfter(ch.Ops)
		if err != nil {
			return fmt.Errorf("%w: %v", approverdevice.ErrPermanent, err)
		}
		var ok bool
		if ceiling, ok = agentadmin.CeilingAfter(pl.Grant, st.CeilingUSD, st.BudgetTotal(), after); !ok {
			return s.refuseOverMaximum(ctx, r, pl, p, actor, after, agentadmin.MaxAllowedTotal(pl.Grant, st.CeilingUSD))
		}
		if p.Status == persistence.ProposalStatusDraft {
			if err := s.proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, actor); err != nil {
				return err
			}
		}
		if why := s.applyAndVerify(ctx, p.ID, actor, ch); why != "" {
			if strings.HasPrefix(why, "a task of this project is running") {
				return errors.New(why) // transient: the tick retries
			}
			return fmt.Errorf("%w: %s", approverdevice.ErrPermanent, why)
		}
	default:
		return fmt.Errorf("%w: the change is %s", approverdevice.ErrPermanent, strings.ToLower(p.Status))
	}
	if err := s.recordGrant(ctx, r, pl, st.CeilingUSD, ceiling); err != nil {
		return err
	}
	// A recipe install's credentials are requested once it applied, each
	// its own approval kind on the phone (design §19.2, §19.11).
	s.fileCredentialFollowUps(ctx, r, pl.Credentials)
	return nil
}

// refuseOverMaximum ends an approved request whose stated maximum the
// namespace sum would now exceed (requests made after it were approved
// first): nothing applies, its proposal is rejected, and the same change is
// filed again rendered against the present, so the user approves the true
// figures (design §18.4 items 2 and 4; the new request's own push reaches
// the approver device). A re-render pins the requests waiting NOW, so a
// finite set of waiting requests yields finitely many re-renders (F4). The
// caller holds s.mu; filing touches only the ledger and the device service's
// repository and push, none of which takes s.mu, so nothing re-enters.
func (s *agentAdminService) refuseOverMaximum(ctx context.Context, r persistence.AgentApprovalRequestRow, pl approvalPayload,
	p *persistence.ControlPlaneProposal, actor string, sum, allowed float64) error {
	// "Its approval stated" only when the figure is the one its sentence
	// named; when a sibling raised the limit above that, the figure is the
	// limit in force (review e1a4 F5).
	figure := "the $" + agentadmin.FormatUSD(allowed) + " its approval stated"
	if stated := statedMaximum(pl.Grant); stated < allowed-1e-9 {
		figure = "the $" + agentadmin.FormatUSD(allowed) + " limit now in force (its approval stated $" + agentadmin.FormatUSD(stated) + ")"
	}
	why := fmt.Sprintf("approving it now would take your assistant's total monthly budget to $%s, above %s, "+
		"because other requests that add spending were approved first; nothing was applied",
		agentadmin.FormatUSD(sum), figure)
	// File the re-render FIRST, then reject (review e1a4 F2): a crash between
	// the two then leaves an approved proposal the retry refuses again, and
	// reRender returns the request it already filed. The other order left a
	// rejected proposal, which the retry answers ErrPermanent without ever
	// filing the re-render.
	next, err := s.reRender(ctx, r, pl, p)
	if err != nil {
		return err // transient: the tick retries, and finds what it already filed
	}
	if p.Status == persistence.ProposalStatusDraft || p.Status == persistence.ProposalStatusApproved {
		if err := s.proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusRejected, actor); err != nil {
			return err // transient: the retry refuses again and finds the re-render
		}
	}
	if next == "" {
		return fmt.Errorf("%w: %s", approverdevice.ErrPermanent, why)
	}
	return fmt.Errorf("%w: %s. Vornik asked again with the true figures: request %s", approverdevice.ErrPermanent, why, next)
}

// statedMaximum is the most a request's sentence stated: max_total_usd, or a
// legacy payload's ceiling_usd; 0 when it stated none.
func statedMaximum(g agentadmin.Grant) float64 {
	switch {
	case g.MaxTotalUSD != nil:
		return *g.MaxTotalUSD
	case g.CeilingUSD != nil:
		return *g.CeilingUSD
	}
	return 0
}

// reRender files the change behind r again, rendered against the present
// state, and returns the new request's ID; "" when the change cannot be
// filed again (its re-render is refused, e.g. the slug was taken meanwhile).
// A request already filed in r's place is returned instead of a second one.
func (s *agentAdminService) reRender(ctx context.Context, r persistence.AgentApprovalRequestRow, pl approvalPayload, p *persistence.ControlPlaneProposal) (string, error) {
	if id, err := s.filedInPlaceOf(ctx, r.Namespace, r.ID); err != nil || id != "" {
		return id, err
	}
	var doc struct {
		Verb  string          `json:"verb"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(pl.Change, &doc); err != nil || doc.Verb == "" {
		return "", nil
	}
	st, err := s.loadStateExcluding(ctx, r.Namespace, agentns.ID(r.Namespace, "home"), r.ID)
	if err != nil {
		return "", err
	}
	ch, err := s.renderer.Render(st, doc.Verb, doc.Input)
	if err != nil || ch.Class != agentadmin.Widening || ch.Slot != nil {
		// Refused (the agent reads why on its next call), or inert, which
		// the refusal makes impossible: the sum exceeded the ceiling in
		// force. Filing nothing is the safe answer to both.
		return "", nil
	}
	np, err := s.fileProposal(ctx, p.ActorKind, p.ActorCredentialID, ch)
	if err != nil {
		return "", err
	}
	res, err := s.fileRequest(ctx, persistence.ApprovalKindWideningChange, np.ID,
		approvalPayload{ProposalID: np.ID, Change: ch.Rendered, Grant: ch.Grant, Narrow: ch.Narrow, Locks: ch.Locks, ReRenderOf: r.ID}, ch)
	if err != nil {
		return "", err
	}
	return res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:], nil
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

// recordGrant writes what an approval grants. The ceiling is written only
// when the apply raised it (from → to, CeilingAfter's decision); the absolute
// ceiling_usd an old payload pinned is never written as is (§18.4).
func (s *agentAdminService) recordGrant(ctx context.Context, r persistence.AgentApprovalRequestRow, pl approvalPayload, from, to float64) error {
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
	for _, m := range pl.Grant.Models {
		// The only writer of agent_model_provider_approvals rows (design
		// §18.6 item 2, round 3 F3; a source test pins it).
		if err := s.grants.UpsertModelDestination(ctx, persistence.AgentModelDestinationApproval{
			Namespace: r.Namespace, Destination: m.Destination, ApprovedByDevice: r.DecidedByDevice, ApprovedAt: at,
		}); err != nil {
			return err
		}
	}
	if to > from {
		if err := s.grants.UpsertCeiling(ctx, persistence.AgentNamespaceBudget{
			Namespace: r.Namespace, CeilingUSD: to, ApprovedByDevice: r.DecidedByDevice, UpdatedAt: at,
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
	return s.loadStateExcluding(ctx, ns, homeProject, "")
}

// loadStateExcluding is loadState with one request left out of the pending
// set: the one being applied, whose locks and spending are its own.
func (s *agentAdminService) loadStateExcluding(ctx context.Context, ns, homeProject, excludeRequest string) (*agentadmin.State, error) {
	cfg := s.c.Config.AgentAdmin
	st := &agentadmin.State{
		Namespace: ns, DefaultBudgetUSD: cfg.EffectiveDefaultProjectBudget(), CeilingUSD: cfg.EffectiveNamespaceBudget(),
		AgentImage: cfg.EffectiveAgentImage(imagemanifest.AgentImageTag),
		Projects:   map[string]*agentadmin.ProjectState{}, Workflows: map[string]*agentadmin.WorkflowState{},
		FileHashes: map[string]string{}, ProjectYAML: map[string][]byte{}, Locked: map[string]bool{},
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
	// The model catalogue, classified against the live router now, and the
	// namespace's live destination approvals (design §18.6 item 2).
	st.UseModelCatalogue(s.c.agentModelSpecs(), s.c.modelRoute, s.c.modelPrice)
	if st.ApprovedDestinations, err = s.c.liveModelDestinations(ctx, ns); err != nil {
		return nil, err
	}
	return st, s.lockPending(ctx, st, ns, excludeRequest)
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
			if dir == "projects" {
				st.ProjectYAML[dir+"/"+e.Name()] = b
			}
		}
	}
	return nil
}

// lockPending marks what pending and approved-unapplied changes hold, and
// sums what the waiting ones add to the namespace's spending (§18.4 item 3:
// widening changes only; a credential slot or a device enrolment adds none).
func (s *agentAdminService) lockPending(ctx context.Context, st *agentadmin.State, ns, exclude string) error {
	pending, err := s.requests.ListPending(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	unapplied, err := s.requests.ListApprovedUnapplied(ctx)
	if err != nil {
		return err
	}
	for _, r := range append(pending, unapplied...) {
		if r.ID == exclude || r.Namespace != ns || (r.Kind != persistence.ApprovalKindWideningChange && r.Kind != persistence.ApprovalKindCredentialSlot) {
			continue
		}
		var pl approvalPayload
		if json.Unmarshal(r.Rendered, &pl) != nil {
			continue
		}
		for _, l := range pl.Locks {
			st.Locked[l] = true
			if st.LockedBy == nil {
				st.LockedBy = map[string]string{}
			}
			st.LockedBy[l] = r.ID + " (" + r.Kind + ")"
		}
		if r.Kind == persistence.ApprovalKindWideningChange && pl.Grant.AddsUSD != nil && *pl.Grant.AddsUSD > 0 {
			st.PendingAddsUSD += *pl.Grant.AddsUSD
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

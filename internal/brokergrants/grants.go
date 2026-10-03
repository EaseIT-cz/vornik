package brokergrants

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Sealer seals and opens a grant's key values; *secretstore.Store is one.
type Sealer interface {
	Seal(ns, label string, plain []byte) (string, error)
	Open(ns, label, sealed string) ([]byte, error)
}

// OperatorSealNamespace seals the key values of operator projects' grants,
// which have no agent namespace. It is a namespace slug only to satisfy the
// secret store's key derivation (the AEAD's additional data also binds the
// grant id).
const OperatorSealNamespace = "operatorgrants"

// ErrNotOffered means the days or uses chosen are not among those offered.
var ErrNotOffered = errors.New("brokergrants: that duration or count is not one this write offers")

// ErrNotEligible means the action's workflow declares no standing for it.
var ErrNotEligible = errors.New("brokergrants: this write cannot be approved for the future")

// Config wires a Service.
type Config struct {
	Grants  persistence.BrokerGrantRepository
	Actions persistence.BrokerActionRepository
	// Proposal finds the live workflow's declaration of an action.
	Proposal func(workflowID, action string) (registry.BrokerProposal, bool)
	// ReachHash is the workflow's current approved reach hash (an agent
	// namespace's device-approved hash, an operator project's live one).
	ReachHash      func(ctx context.Context, projectID, workflowID string) (string, error)
	SealerForWrite func() (Sealer, error)
	SealerForRead  func() (Sealer, error)
	// Bounds are the daemon's (days, uses, live per project).
	Bounds  func() (days, uses, live int)
	Metrics *Metrics
	Now     func() time.Time
	Logger  zerolog.Logger
	// Notify pushes a digest: ns is the agent namespace, "" for operator
	// projects; n the covered writes since the last digest.
	Notify func(ctx context.Context, ns string, n int)
}

// Service is the standing-grant logic over the store.
type Service struct{ cfg Config }

// New builds a Service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Bounds == nil {
		cfg.Bounds = func() (int, int, int) {
			return registry.StandingMaxDays, registry.StandingMaxUses, registry.StandingMaxLivePerProject
		}
	}
	return &Service{cfg: cfg}
}

// SetNotify replaces the digest push.
func (s *Service) SetNotify(fn func(ctx context.Context, ns string, n int)) { s.cfg.Notify = fn }

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// Offer is what the approval page offers under an eligible write (item 2).
type Offer struct {
	// Action names the write ("send reply").
	Action string
	// Key is the normalised key ("to a@x.com, cc (none)").
	Key string
	// Days and Uses are the choices, ascending.
	Days, Uses []int
	// Unreviewed are the arguments a covered write sends unseen.
	Unreviewed []string
}

// Offer returns the grant a person may create while approving a, or nil
// when its workflow declares no standing for it (or the key cannot be
// computed). The key shown is the action's own, normalised: no stored grant
// is opened.
func (s *Service) Offer(a *persistence.BrokerAction) *Offer {
	if a == nil {
		return nil
	}
	p, ok := s.proposal(a)
	if !ok {
		return nil
	}
	key, err := KeyOf(p.Standing.Key, p.Destinations(), a.ArgsJSON)
	if err != nil {
		return nil
	}
	cfgDays, cfgUses, _ := s.cfg.Bounds()
	maxDays := min(p.Standing.EffectiveMaxDays(), cfgDays)
	maxUses := min(p.Standing.EffectiveMaxUses(), cfgUses)
	if maxDays < 1 || maxUses < 1 {
		return nil
	}
	o := &Offer{Action: humanAction(a.ActionKind), Key: key.Display(),
		Days: distinct(1, maxDays), Uses: distinct(min(5, maxUses), maxUses)}
	inKey := map[string]bool{}
	for _, k := range p.Standing.Key {
		inKey[k] = true
	}
	props, _ := p.ArgsSchema["properties"].(map[string]any)
	for name := range props {
		if !inKey[name] {
			o.Unreviewed = append(o.Unreviewed, name)
		}
	}
	sort.Strings(o.Unreviewed)
	return o
}

func distinct(a, b int) []int {
	if a == b {
		return []int{a}
	}
	return []int{a, b}
}

func humanAction(kind string) string {
	out := []rune(kind)
	for i, r := range out {
		if r == '_' {
			out[i] = ' '
		}
	}
	return string(out)
}

func (s *Service) proposal(a *persistence.BrokerAction) (registry.BrokerProposal, bool) {
	if s.cfg.Proposal == nil {
		return registry.BrokerProposal{}, false
	}
	p, ok := s.cfg.Proposal(a.WorkflowID, a.ActionKind)
	if !ok || p.Standing == nil || p.Tool != a.Tool {
		return registry.BrokerProposal{}, false
	}
	return p, true
}

// sealNamespace is the secret-store namespace a project's grants seal under.
func sealNamespace(projectID string) (seal, ns string) {
	if ns, ok := agentns.FromID(projectID); ok {
		return ns, ns
	}
	return OperatorSealNamespace, ""
}

func sealLabel(grantID string) string { return "grant/" + grantID }

// GrantID derives a grant's id from its seed action, so a re-applied
// approval effect cannot create a second grant.
func GrantID(seedActionID string) string { return "bsg_" + seedActionID }

// ApproveWithGrant approves a (bound to shownSHA) and creates its grant in
// one transaction (item 2). days and uses must be among Offer's choices.
// The key is computed by KeyOf, the one canonicalisation, over the action's
// own arguments; its canonical JSON is sealed, its hash stored.
func (s *Service) ApproveWithGrant(ctx context.Context, a *persistence.BrokerAction, shownSHA, approver string, days, uses int) (*persistence.BrokerStandingGrant, error) {
	o := s.Offer(a)
	if o == nil {
		return nil, ErrNotEligible
	}
	if !contains(o.Days, days) || !contains(o.Uses, uses) {
		return nil, ErrNotOffered
	}
	p, _ := s.proposal(a)
	key, err := KeyOf(p.Standing.Key, p.Destinations(), a.ArgsJSON)
	if err != nil {
		return nil, err
	}
	reach, err := s.cfg.ReachHash(ctx, a.ProjectID, a.WorkflowID)
	if err != nil {
		return nil, fmt.Errorf("brokergrants: the workflow's approved reach could not be read: %w", err)
	}
	sealer, err := s.cfg.SealerForWrite()
	if err != nil {
		return nil, fmt.Errorf("brokergrants: the seal is not available: %w", err)
	}
	id := GrantID(a.ActionID)
	sealNS, ns := sealNamespace(a.ProjectID)
	sealed, err := sealer.Seal(sealNS, sealLabel(id), key.Canonical)
	if err != nil {
		return nil, err
	}
	now := s.now()
	_, _, maxLive := s.cfg.Bounds()
	g := &persistence.BrokerStandingGrant{
		ID: id, ProjectID: a.ProjectID, Namespace: ns, WorkflowID: a.WorkflowID, Action: a.ActionKind,
		KeyPaths: append([]string(nil), p.Standing.Key...), KeyValuesSealed: sealed, KeyHash: key.Hash,
		MaxUses: uses, UsesLeft: uses, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour), CreatedAt: now,
		CreatedBy: approver, SeedActionID: a.ActionID, ReachHashAtCreation: reach, Active: true, DigestThrough: now,
	}
	if err := s.cfg.Grants.ApproveSeedAndCreate(ctx, a.ActionID, shownSHA, approver, g, maxLive, now); err != nil {
		return nil, err
	}
	return g, nil
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// openKey opens a grant's sealed key. Two functions call it, and a source
// test pins that: matchingGrant (the comparison) and Views (the grant
// pages). Nothing returns its result to an agent.
func (s *Service) openKey(g *persistence.BrokerStandingGrant) ([]byte, error) {
	sealer, err := s.cfg.SealerForRead()
	if err != nil {
		return nil, err
	}
	sealNS, _ := sealNamespace(g.ProjectID)
	return sealer.Open(sealNS, sealLabel(g.ID), g.KeyValuesSealed)
}

// Cover approves a pending action under a matching grant, or reports that
// none covers it (the caller then files the ordinary per-write approval).
// Matching only chooses which grant to try; the guarded decrement decides.
func (s *Service) Cover(ctx context.Context, a *persistence.BrokerAction) (bool, error) {
	if a == nil || a.Status != persistence.BrokerActionPending {
		return false, nil
	}
	p, ok := s.proposal(a)
	if !ok {
		return false, nil
	}
	key, err := KeyOf(p.Standing.Key, p.Destinations(), a.ArgsJSON)
	if err != nil {
		// Visible, not silent (review f819): the error names the argument,
		// never its value.
		s.cfg.Logger.Warn().Err(err).Str("action_id", a.ActionID).Str("workflow", a.WorkflowID).
			Msg("standing grant: the write's key could not be computed; it needs a per-write approval")
		s.cfg.Metrics.recordMiss(MissUnkeyed)
		return false, nil
	}
	g, miss, err := s.matchingGrant(ctx, a, key)
	if err != nil || g == nil {
		s.cfg.Metrics.recordMiss(miss)
		return false, err
	}
	err = s.cfg.Grants.ApproveUnderGrant(ctx, a.ActionID, a.ArgsSHA256, g.ID, key.Hash, s.now())
	switch {
	case errors.Is(err, persistence.ErrBrokerGrantNotCovered):
		s.cfg.Metrics.recordMiss(s.missReason(ctx, g.ID))
		return false, nil
	case errors.Is(err, persistence.ErrBrokerActionNoTransition):
		return false, nil // already decided, or expired: nothing to cover
	case err != nil:
		return false, err
	}
	s.cfg.Metrics.recordCovered(a.ProjectID)
	return true, nil
}

// matchingGrant is the matcher (round 3 step i): the class's grants, their
// keys opened and compared with the action's canonical key; among matches,
// the sooner-expiring usable one (item 7). A grant whose workflow's reach
// changed since it was created is suspended here, on the next action (item
// 6, round 4 F4). miss names why a matched grant could not be used.
func (s *Service) matchingGrant(ctx context.Context, a *persistence.BrokerAction, key Key) (*persistence.BrokerStandingGrant, string, error) {
	cands, err := s.cfg.Grants.ListForAction(ctx, a.ProjectID, a.WorkflowID, a.ActionKind)
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	miss := ""
	reach, reachRead := "", false
	for _, g := range cands { // sooner expiry first
		plain, err := s.openKey(g)
		if err != nil {
			s.cfg.Logger.Error().Err(err).Str("grant", g.ID).Msg("standing grant: its key could not be opened; it covers nothing")
			continue
		}
		if string(plain) != string(key.Canonical) || g.KeyHash != key.Hash {
			continue
		}
		if why := unusable(g, now); why != "" {
			if miss == "" {
				miss = why
			}
			continue
		}
		if !reachRead {
			reach, err = s.cfg.ReachHash(ctx, a.ProjectID, a.WorkflowID)
			if err != nil {
				return nil, "", err
			}
			reachRead = true
		}
		if g.ReachHashAtCreation != reach {
			// The grant covers nothing either way; it is counted suspended
			// only when the suspension really happened (review f819).
			why := MissSuspended
			switch ok, err := s.cfg.Grants.Suspend(ctx, g.ID, now); {
			case err != nil:
				s.cfg.Logger.Error().Err(err).Str("grant", g.ID).Msg("standing grant: its workflow's reach changed, but it could not be suspended; it covers nothing")
				why = MissSuspendFailed
			case ok:
				s.cfg.Metrics.recordSuspended()
			}
			if miss == "" {
				miss = why
			}
			continue
		}
		return g, "", nil
	}
	return nil, miss, nil
}

// unusable names why a grant cannot cover anything now, or "".
func unusable(g *persistence.BrokerStandingGrant, now time.Time) string {
	switch {
	case !g.Active:
		return MissRevoked
	case !g.ExpiresAt.After(now):
		return MissExpired
	case g.UsesLeft <= 0:
		return MissUsed
	case g.Paused:
		return MissPaused
	case g.SuspendedAt != nil:
		return MissSuspended
	}
	return ""
}

// missReason re-reads a grant whose decrement refused, to say why.
func (s *Service) missReason(ctx context.Context, id string) string {
	g, err := s.cfg.Grants.Get(ctx, id)
	if err != nil {
		return MissKey
	}
	if why := unusable(g, s.now()); why != "" {
		return why
	}
	return MissKey
}

// Pause pauses or unpauses a grant (item 8): effective on the next action.
func (s *Service) Pause(ctx context.Context, id string, paused bool) error {
	return s.cfg.Grants.SetPaused(ctx, id, paused)
}

// Revoke ends a grant (item 8): final, effective on the next action. A send
// already executing completes.
func (s *Service) Revoke(ctx context.Context, id string) error {
	if err := s.cfg.Grants.Revoke(ctx, id, s.now()); err != nil {
		return err
	}
	s.cfg.Metrics.recordRevoked()
	return nil
}

// Confirm clears a suspension, re-pinning the workflow's current reach hash
// for this grant only (item 6).
func (s *Service) Confirm(ctx context.Context, id string) error {
	g, err := s.cfg.Grants.Get(ctx, id)
	if err != nil {
		return err
	}
	reach, err := s.cfg.ReachHash(ctx, g.ProjectID, g.WorkflowID)
	if err != nil {
		return err
	}
	return s.cfg.Grants.Confirm(ctx, id, reach)
}

// Verbs are the changes a person may make from a Standing approvals page.
var Verbs = map[string]bool{"pause": true, "unpause": true, "revoke": true, "confirm": true}

// Change applies one page verb: the one switch both pages (the device's and
// /inbox) call.
func (s *Service) Change(ctx context.Context, id, verb string) error {
	switch verb {
	case "pause":
		return s.Pause(ctx, id, true)
	case "unpause":
		return s.Pause(ctx, id, false)
	case "revoke":
		return s.Revoke(ctx, id)
	case "confirm":
		return s.Confirm(ctx, id)
	}
	return fmt.Errorf("brokergrants: unknown change %q", verb)
}

// Get loads one grant (for the pages' scope checks).
func (s *Service) Get(ctx context.Context, id string) (*persistence.BrokerStandingGrant, error) {
	return s.cfg.Grants.Get(ctx, id)
}

// View is one grant as the Standing approvals page shows it.
type View struct {
	Grant   *persistence.BrokerStandingGrant
	Key     string
	State   string // active, paused, suspended, revoked, expired, used
	Covered []*persistence.BrokerAction
}

// Views lists grants for a page, keys opened (the second opener).
func (s *Service) Views(ctx context.Context, f persistence.BrokerGrantFilter) ([]View, error) {
	gs, err := s.cfg.Grants.List(ctx, f)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]View, 0, len(gs))
	for _, g := range gs {
		v := View{Grant: g, State: "active", Key: "(cannot be shown: the key could not be opened)"}
		if why := unusable(g, now); why != "" {
			v.State = why
		}
		if plain, err := s.openKey(g); err == nil {
			if k, err := keyFromCanonical(g.KeyPaths, plain); err == nil {
				v.Key = k.Display()
			}
		}
		if v.Covered, err = s.cfg.Grants.CoveredActions(ctx, g.ID, 50); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Digest runs one digest pass (item 8): every grant whose last digest is a
// day old has its window advanced (a compare-and-set, so two nodes never
// count one window twice) and its covered writes counted; one push per
// scope (agent namespace, or operator projects) with the total.
func (s *Service) Digest(ctx context.Context) error {
	now := s.now()
	due, err := s.cfg.Grants.DigestDue(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return err
	}
	totals := map[string]int{}
	var order []string
	for _, g := range due {
		n, ok, err := s.cfg.Grants.AdvanceDigest(ctx, g.ID, g.DigestThrough, now)
		if err != nil {
			return err
		}
		if !ok || n == 0 {
			continue
		}
		if _, seen := totals[g.Namespace]; !seen {
			order = append(order, g.Namespace)
		}
		totals[g.Namespace] += n
	}
	if s.cfg.Notify != nil {
		for _, ns := range order {
			s.cfg.Notify(ctx, ns, totals[ns])
		}
	}
	return nil
}

// RefreshGauges replaces the live and paused gauges.
func (s *Service) RefreshGauges(ctx context.Context) error {
	rows, err := s.cfg.Grants.CountLive(ctx, s.now())
	if err != nil {
		return err
	}
	s.cfg.Metrics.SetLive(rows)
	return nil
}

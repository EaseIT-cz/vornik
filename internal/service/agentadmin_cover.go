package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// The cover request (agent-administered design §18.4 F10). Incident
// 2026-10-02: namespace claudecode's projects summed $108 against a $104
// approved ceiling (the pre-§18.4 race); an operator lowering the ceiling
// reaches the same state. While a namespace is above its ceiling:
//
//   - any admin verb it makes, read or write, files ONE cover request when
//     none waits ("approving sets the limit to the sum"), from the state the
//     verb loaded under s.mu, before the verb's block check, so the first
//     refusal names the request it filed;
//   - no new one is filed within coverCooldown of the namespace's most
//     recent cover request being rejected, read from the approval table;
//   - every change that would raise the namespace sum is refused, naming the
//     waiting cover request.
//
// The cover request is filed directly, never through Renderer.Render: its
// proposal carries no file operation, and an empty change rendered there
// could be classified inert and applied without the device (2b25 F6).

// coverCooldown is how long a rejected cover request silences filing.
const coverCooldown = 24 * time.Hour

// coverVerb names the cover request's change in its rendered document.
const coverVerb = "cover_namespace_total"

// AgentCoverActor proposes the cover request's no-op change.
const AgentCoverActor = "system:agent-admin-cover"

// coverSentenceOf is the cover request's sentence. It names no cause: there
// are two (2b25 F1).
func coverSentenceOf(total, ceiling float64) string {
	t, c := agentadmin.FormatUSD(total), agentadmin.FormatUSD(ceiling)
	return "Your assistant's projects add up to $" + t + " a month, above the $" + c + " limit now in force. " +
		"Approving sets the limit to $" + t + ". To keep $" + c + " instead, reject this and ask your assistant to lower a budget."
}

// isCover reports whether a request is a cover request. The flag is read
// from the canonical document the device approved.
func isCover(r persistence.AgentApprovalRequestRow) bool {
	var pl approvalPayload
	if r.Kind != persistence.ApprovalKindWideningChange || json.Unmarshal(r.Rendered, &pl) != nil {
		return false
	}
	return pl.Grant.Cover
}

// waitingCover returns the namespace's cover request that waits for a
// decision or for its effect (pending, or approved and unapplied), or "".
func waitingCover(ctx context.Context, repo persistence.ApproverDeviceRepository, ns string) (string, error) {
	pending, err := repo.ListPending(ctx, time.Now().UTC())
	if err != nil {
		return "", err
	}
	unapplied, err := repo.ListApprovedUnapplied(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range append(pending, unapplied...) {
		if r.Namespace != ns {
			continue
		}
		if isCover(r) {
			return r.ID, nil
		}
	}
	return "", nil
}

// ensureCover files the namespace's cover request when it is above its
// ceiling, none waits, and none was rejected within coverCooldown. It returns
// the waiting cover request's ID ("" when none waits). The caller holds s.mu
// and loaded st under it, so two concurrent verbs file one request.
func (s *agentAdminService) ensureCover(ctx context.Context, st *agentadmin.State) (string, error) {
	total := st.BudgetTotal()
	if total <= st.CeilingUSD+1e-9 {
		return "", nil
	}
	id, err := waitingCover(ctx, s.requests, st.Namespace)
	if err != nil || id != "" {
		return id, err
	}
	cooling, err := s.coverCoolingDown(ctx, st.Namespace)
	if err != nil || cooling {
		return "", err
	}
	return s.fileCover(ctx, st.Namespace, total, st.CeilingUSD, "")
}

// coverCoolingDown reports whether the namespace's most recent cover request
// was rejected less than coverCooldown ago. Durable: it reads the approval
// table, so a restart changes nothing (round 3 F6). A cover request is
// decided within RequestTTL of being filed, so older rows cannot matter.
func (s *agentAdminService) coverCoolingDown(ctx context.Context, ns string) (bool, error) {
	now := s.now().UTC()
	rows, err := s.requests.ListRecentByNamespace(ctx, ns, now.Add(-coverCooldown-approverdevice.RequestTTL))
	if err != nil {
		return false, err
	}
	for _, r := range rows { // newest first
		if !isCover(r) {
			continue
		}
		return r.Status == persistence.ApprovalRejected && r.DecidedAt != nil && now.Sub(*r.DecidedAt) < coverCooldown, nil
	}
	return false, nil
}

// fileCover files a cover request pinning total: a proposal with no file
// operation and a grant {cover, adds 0, max_total = total}. reRenderOf names
// the cover request it replaces, "" for a first one.
func (s *agentAdminService) fileCover(ctx context.Context, ns string, total, ceiling float64, reRenderOf string) (string, error) {
	sentence := coverSentenceOf(total, ceiling)
	change, err := json.Marshal(map[string]any{"verb": coverVerb, "namespace": ns, "total_usd": total, "ceiling_usd": ceiling})
	if err != nil {
		return "", err
	}
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), Kind: persistence.ProposalKindScaffold, BlastRadius: persistence.ProposalScopeProject,
		Title: truncate(sentence, 200), ApplyOps: "[]", Evidence: `{"read_set":{}}`,
		Status: persistence.ProposalStatusDraft, ProposedBy: AgentCoverActor,
		Entrypoint: "agent", ActorKind: AgentSystemActor,
	}
	if err := s.proposals.Create(ctx, p); err != nil {
		return "", fmt.Errorf("file the cover request: %w", err)
	}
	zero, maxTotal := 0.0, total
	res, err := s.fileRequest(ctx, persistence.ApprovalKindWideningChange, p.ID, approvalPayload{
		ProposalID: p.ID, Change: change, ReRenderOf: reRenderOf,
		Grant: agentadmin.Grant{Cover: true, AddsUSD: &zero, MaxTotalUSD: &maxTotal},
	}, agentadmin.Change{Namespace: ns, Sentence: sentence})
	if err != nil {
		return "", err
	}
	return requestIDOfURL(res.ApprovalURL), nil
}

// requestIDOfURL is the request ID an approval link ends in.
func requestIDOfURL(url string) string { return url[strings.LastIndex(url, "/")+1:] }

// approvalURL is the device page of a request.
func (s *agentAdminService) approvalURL(id string) string {
	return strings.TrimRight(s.c.Config.PublicOrigin(), "/") + "/ui/approve/" + id
}

// spendingBlocked returns why a change is refused while the namespace is
// above its ceiling: it raises the namespace sum (State.BudgetTotalAfter
// above the total before; the predicate is the effect, not the verb, round 3
// F1). "" lets the change proceed: one that keeps or lowers the sum, or any
// change while the namespace is within its ceiling.
func (s *agentAdminService) spendingBlocked(st *agentadmin.State, ch agentadmin.Change, coverID string) (string, error) {
	before := st.BudgetTotal()
	if before <= st.CeilingUSD+1e-9 {
		return "", nil
	}
	after, err := st.BudgetTotalAfter(ch.Ops)
	if err != nil {
		return "", err
	}
	if after <= before+1e-9 {
		return "", nil
	}
	why := fmt.Sprintf("%s refused: your assistant's projects already add up to $%s a month, above the $%s limit now in force, "+
		"so no change that adds spending can be made until that is settled", ch.Verb, agentadmin.FormatUSD(before), agentadmin.FormatUSD(st.CeilingUSD))
	if coverID != "" {
		return why + "; the user can approve the higher limit, or reject it and ask you to lower a budget, at " + s.approvalURL(coverID), nil
	}
	return why + "; the user recently declined to raise the limit, so lower a budget first", nil
}

// coverOnRead files the cover request a read verb owes (§18.4 F10 item 3):
// the operator learns of the gap from the next thing the assistant does. So
// in an over-ceiling namespace a read writes a request row and pushes to the
// device (synchronously, under s.mu), and fails if the approval store cannot
// be read: fail-closed, at the cost of the read's availability while the
// store is down (review 10b7 F3).
func (s *agentAdminService) coverOnRead(ctx context.Context, key *persistence.APIKey) error {
	if key == nil || !key.AgentAdmin || !agentns.Valid(key.AgentNamespace) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadState(ctx, key.AgentNamespace, key.ProjectID)
	if err != nil {
		return err
	}
	_, err = s.ensureCover(ctx, st)
	return err
}

// applyCover is the effect of an approved cover request. The caller holds
// s.mu and loaded st under it, leaving r out of the pending set. Only an
// exact match of the sum now and the sum the sentence stated raises the
// ceiling, to that sum (2b25 F2). A sum within the ceiling fails the request
// as moot; any other sum re-files the request with the true figure first and
// fails this one second, so a crash between the two leaves one replacement
// (round 3 F3). The proposal goes DRAFT → APPROVED as the device and is
// marked applied, as for any change; only the config engine and the load
// check are skipped, there being nothing to apply (2b25 F5).
func (s *agentAdminService) applyCover(ctx context.Context, r persistence.AgentApprovalRequestRow, pl approvalPayload,
	p *persistence.ControlPlaneProposal, actor string, st *agentadmin.State) error {
	if pl.Grant.MaxTotalUSD == nil {
		return fmt.Errorf("%w: the cover request states no total", approverdevice.ErrPermanent)
	}
	stated, total, ceiling := *pl.Grant.MaxTotalUSD, st.BudgetTotal(), st.CeilingUSD
	switch p.Status {
	case persistence.ProposalStatusApplied:
		// Marked applied before a crash: write what the approval covers. The
		// min() is the sum now, never above the stated figure; it equals the
		// stated figure because only the equality branch marks a cover applied
		// and spending raises are blocked while over (spendingBlocked). A
		// change that let the sum rise here would make this under-record
		// (review 10b7 F4).
		return s.recordGrant(ctx, r, pl, ceiling, math.Max(ceiling, math.Min(total, stated)))
	case persistence.ProposalStatusDraft, persistence.ProposalStatusApproved:
	default:
		return fmt.Errorf("%w: the change is %s", approverdevice.ErrPermanent, strings.ToLower(p.Status))
	}
	reject := func() error {
		return s.proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusRejected, actor)
	}
	if total <= ceiling+1e-9 {
		if err := reject(); err != nil {
			return err
		}
		return fmt.Errorf("%w: the projects now add up to $%s, within the $%s limit; nothing changed",
			approverdevice.ErrPermanent, agentadmin.FormatUSD(total), agentadmin.FormatUSD(ceiling))
	}
	if math.Abs(total-stated) > 1e-9 {
		next, err := s.filedInPlaceOf(ctx, r.Namespace, r.ID)
		if err != nil {
			return err
		}
		if next == "" {
			if next, err = s.fileCover(ctx, r.Namespace, total, ceiling, r.ID); err != nil {
				return err // transient: the retry files it
			}
		}
		if err := reject(); err != nil {
			return err // transient: the retry finds the replacement
		}
		return fmt.Errorf("%w: your assistant's projects now add up to $%s a month, not the $%s this request stated; nothing was changed. "+
			"Vornik asked again with the true figures: request %s",
			approverdevice.ErrPermanent, agentadmin.FormatUSD(total), agentadmin.FormatUSD(stated), next)
	}
	if p.Status == persistence.ProposalStatusDraft {
		if err := s.proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, actor); err != nil {
			return err
		}
	}
	if err := s.proposals.MarkApplied(ctx, p.ID, actor, ""); err != nil && !errors.Is(err, persistence.ErrProposalNotApproved) {
		return err
	}
	return s.recordGrant(ctx, r, pl, ceiling, total)
}

// filedInPlaceOf returns the waiting widening_change request filed in place
// of request id (its payload's re_render_of), or "".
func (s *agentAdminService) filedInPlaceOf(ctx context.Context, ns, id string) (string, error) {
	pending, err := s.requests.ListPending(ctx, time.Now().UTC())
	if err != nil {
		return "", err
	}
	for _, q := range pending {
		var qp approvalPayload
		if q.Namespace == ns && q.Kind == persistence.ApprovalKindWideningChange &&
			json.Unmarshal(q.Rendered, &qp) == nil && qp.ReRenderOf == id {
			return q.ID, nil
		}
	}
	return "", nil
}

// agentNamespaceBudgets is the agent_namespace_budget doctor check's source:
// every agent namespace with a loaded project, its budget sum, the ceiling
// in force and its waiting cover request. It reads the registry and the
// approval tables directly, so it works before the verbs are built.
func (c *Container) agentNamespaceBudgets(ctx context.Context) ([]api.AgentNamespaceBudgetStatus, error) {
	if c.Registry == nil || c.repos == nil || c.repos.AgentGrants == nil || c.repos.ApproverDevices == nil {
		return nil, errors.New("the registry or the approval tables are not available")
	}
	totals := map[string]float64{}
	for _, p := range c.Registry.ListProjects() {
		if ns, ok := agentns.FromID(p.ID); ok {
			totals[ns] += p.Budget.MonthlyHardUSD
		}
	}
	out := make([]api.AgentNamespaceBudgetStatus, 0, len(totals))
	for ns, total := range totals {
		ceiling := c.Config.AgentAdmin.EffectiveNamespaceBudget()
		if b, err := c.repos.AgentGrants.GetCeiling(ctx, ns); err == nil {
			ceiling = b.CeilingUSD
		} else if !errors.Is(err, persistence.ErrNotFound) {
			return nil, err
		}
		cover, err := waitingCover(ctx, c.repos.ApproverDevices, ns)
		if err != nil {
			return nil, err
		}
		out = append(out, api.AgentNamespaceBudgetStatus{Namespace: ns, TotalUSD: total, CeilingUSD: ceiling, CoverRequestID: cover})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out, nil
}

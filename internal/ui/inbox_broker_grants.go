package ui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/brokergrants"
	"vornik.io/vornik/internal/persistence"
)

// Standing grants in /inbox for operator projects (broker write-actions
// design, "Tier 2: standing grants" as revised, items 2 and 8). An agent
// project's writes, and so its grants, are decided on the approver device
// only.

// WithStandingGrants wires the standing-grant service. The provider is read
// at call time; nil (or a nil service) hides the offer and the section.
func WithStandingGrants(provider func() *brokergrants.Service) ServerOption {
	return func(s *Server) { s.standingGrants = provider }
}

func (s *Server) grantService() *brokergrants.Service {
	if s.standingGrants == nil {
		return nil
	}
	return s.standingGrants()
}

// grantOfferFor is a card's offer, or nil.
func (s *Server) grantOfferFor(row *persistence.BrokerAction) *brokergrants.Offer {
	svc := s.grantService()
	if svc == nil || row == nil {
		return nil
	}
	if _, agent := agentns.FromID(row.ProjectID); agent {
		return nil
	}
	return svc.Offer(row)
}

// BrokerActionApproveWithGrant approves a pending write and creates its
// standing grant in one transaction, through the same gate as a single
// approve (brokerActionGate: same origin before any read, then project scope
// and the approver).
func (s *Server) BrokerActionApproveWithGrant(w http.ResponseWriter, r *http.Request, actionID string) {
	row, approver, ok := s.brokerActionGate(w, r, actionID)
	if !ok {
		return
	}
	// The gate already refuses an agent project's write; the grant handler
	// says so itself as well (review 3bed): an agent namespace's grant is
	// made on the approver device only.
	if _, agent := agentns.FromID(row.ProjectID); agent {
		http.Error(w, "approve this on your approver device", http.StatusForbidden)
		return
	}
	svc := s.grantService()
	if svc == nil {
		http.Error(w, "standing approvals are not configured", http.StatusServiceUnavailable)
		return
	}
	days, err1 := strconv.Atoi(r.FormValue("grant_days"))
	uses, err2 := strconv.Atoi(r.FormValue("grant_uses"))
	if err1 != nil || err2 != nil {
		http.Error(w, "choose how long and how many times", http.StatusBadRequest)
		return
	}
	shown := strings.TrimSpace(r.FormValue("args_sha256"))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_, err := svc.ApproveWithGrant(ctx, row, shown, approver, days, uses)
	switch {
	case errors.Is(err, brokergrants.ErrNotOffered), errors.Is(err, brokergrants.ErrNotEligible):
		http.Error(w, "that standing approval is not offered for this write", http.StatusBadRequest)
		return
	case errors.Is(err, persistence.ErrBrokerActionNoTransition):
		s.redirectInbox(w, r, "broker-action-not-approvable")
		return
	case errors.Is(err, persistence.ErrBrokerGrantLimit):
		s.redirectInbox(w, r, "standing-grant-limit")
		return
	case err != nil:
		s.logger.Error().Err(err).Str("action_id", actionID).Msg("broker action approve with a standing grant failed")
		http.Error(w, "approve failed", http.StatusInternalServerError)
		return
	}
	s.logger.Info().Str("action_id", actionID).Str("project", row.ProjectID).Str("approver", approver).
		Int("days", days).Int("uses", uses).Msg("broker action approved with a standing grant")
	if s.brokerActionKick != nil {
		s.brokerActionKick(actionID)
	}
	s.signalBrokerActionChanged()
	s.redirectInbox(w, r, "broker-action-approved")
}

// standingCard is one operator grant in the inbox's Standing approvals.
type standingCard struct {
	ID, ProjectID, Workflow, Action, Key, State string
	UsesLeft, MaxUses                           int
	Expires                                     string
	Covered                                     []*persistence.BrokerAction
}

// loadStandingGrants lists the operator grants in the request's scope.
func (s *Server) loadStandingGrants(r *http.Request) []standingCard {
	svc := s.grantService()
	if svc == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	views, err := svc.Views(ctx, persistence.BrokerGrantFilter{ProjectIDs: scopeQueryIDs(r)})
	if err != nil {
		s.logger.Warn().Err(err).Msg("inbox: standing approvals could not be listed")
		return nil
	}
	var out []standingCard
	for _, v := range views {
		if !api.RequestAllowsProject(r, v.Grant.ProjectID) {
			continue
		}
		out = append(out, standingCard{ID: v.Grant.ID, ProjectID: v.Grant.ProjectID, Workflow: v.Grant.WorkflowID,
			Action: v.Grant.Action, Key: v.Key, State: v.State, UsesLeft: v.Grant.UsesLeft, MaxUses: v.Grant.MaxUses,
			Expires: v.Grant.ExpiresAt.UTC().Format("2 Jan 2006 15:04 UTC") + " (" + v.Grant.ExpiresAt.Local().Format("15:04 MST") + " local)",
			Covered: v.Covered})
	}
	return out
}

// StandingGrantChange serves POST /ui/inbox/standing/<id>/<verb>: pause,
// unpause, revoke or confirm an operator project's grant. Same origin
// before any read; then the grant's project scope and the approver.
func (s *Server) StandingGrantChange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := approval.CheckRequest(r); err != nil {
		approval.WriteError(w, r, err)
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/ui"), "/inbox/standing/")
	id, verb, ok := strings.Cut(rest, "/")
	svc := s.grantService()
	if !ok || id == "" || svc == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	g, err := svc.Get(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if g.Namespace != "" {
		http.Error(w, "change this on your approver device", http.StatusForbidden)
		return
	}
	if _, err := approval.Authorize(r, g.ProjectID, api.RequestAllowsProject, s.operatorIDForRequest); err != nil {
		approval.WriteError(w, r, err)
		return
	}
	if !brokergrants.Verbs[verb] {
		http.NotFound(w, r)
		return
	}
	if err := svc.Change(ctx, id, verb); err != nil {
		s.redirectInbox(w, r, "standing-grant-unchanged")
		return
	}
	s.redirectInbox(w, r, "standing-grant-"+verb)
}

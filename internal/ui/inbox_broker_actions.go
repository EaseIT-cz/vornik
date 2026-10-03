package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/brokergrants"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/persistence"
)

// Broker write actions in /inbox — https://docs.vornik.io
// 2026-09-29-broker-write-actions-and-push-design.md §5.3. A pending action
// is a write a broker workflow proposed for a front-end agent. The operator
// sees the exact arguments, and approving binds to the hash of what was
// rendered; the daemon's action worker then executes those bytes.

// WithBrokerActions wires the broker-action store and the worker's kick.
// Optional: nil leaves the cards hidden and the routes unconfigured.
func WithBrokerActions(repo persistence.BrokerActionRepository, kick func(actionID string)) ServerOption {
	return func(s *Server) {
		s.brokerActionRepo = repo
		s.brokerActionKick = kick
	}
}

// WithBrokerActionChanged wires the push outbox's kick, told after a
// successful approve or reject (broker write-actions design §7a). Optional.
func WithBrokerActionChanged(fn func()) ServerOption {
	return func(s *Server) { s.brokerActionChanged = fn }
}

func (s *Server) signalBrokerActionChanged() {
	if s.brokerActionChanged != nil {
		s.brokerActionChanged()
	}
}

type brokerActionCard struct {
	ActionID   string
	ProjectID  string
	TaskID     string
	APIKeyID   string
	ActionKind string
	Tool       string
	ArgsPretty string
	ArgsSHA256 string
	// KeyLabel is the front agent's key name, or its id when the key
	// store is unwired or the key is gone.
	KeyLabel string
	// UntrustedFields are the argument paths the workflow's args_schema
	// marks x-untrusted: text a model drafted after reading third-party
	// content. Empty when the workflow no longer declares the action.
	UntrustedFields []string
	// SchemaUnavailable: the workflow no longer declares this action (or
	// the registry is unwired), so which fields are free text is unknown.
	// The card says so rather than flagging nothing (review-20260930-1334 F2).
	SchemaUnavailable bool
	Expires           string
	Age               string
	createdAt         time.Time
	// Grant is the standing grant offered under Approve (broker
	// write-actions design, tier 2), or nil.
	Grant *brokergrants.Offer
}

// loadPendingBrokerActions lists pending actions in the request's scope.
// Every failure degrades to fewer cards, never to a failed inbox.
func (s *Server) loadPendingBrokerActions(r *http.Request) []brokerActionCard {
	if s.brokerActionRepo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	projects := scopeQueryIDs(r)
	if projects == nil {
		projects = []string{""} // unscoped: every project
	}
	var cards []brokerActionCard
	for _, p := range projects {
		rows, err := s.brokerActionRepo.ListByStatus(ctx, p, persistence.BrokerActionPending, 100)
		if err != nil {
			s.logger.Warn().Err(err).Str("project", p).Msg("inbox: pending broker actions list failed; that project's cards suppressed")
			continue
		}
		now := time.Now()
		for _, row := range rows {
			if row == nil || !api.RequestAllowsProject(r, row.ProjectID) {
				continue
			}
			if _, agent := agentns.FromID(row.ProjectID); agent {
				continue // approved on the approver device only (plan P4.8)
			}
			// Expired but not yet swept: not approvable, so no card
			// (review-20260930-1334 F4).
			if !row.ExpiresAt.After(now) {
				continue
			}
			cards = append(cards, s.newBrokerActionCard(ctx, row))
		}
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].createdAt.Before(cards[j].createdAt) })
	return cards
}

func (s *Server) newBrokerActionCard(ctx context.Context, row *persistence.BrokerAction) brokerActionCard {
	pretty := string(row.ArgsJSON)
	var buf bytes.Buffer
	if err := json.Indent(&buf, row.ArgsJSON, "", "  "); err == nil {
		pretty = buf.String()
	}
	card := brokerActionCard{
		ActionID: row.ActionID, ProjectID: row.ProjectID, TaskID: row.TaskID, APIKeyID: row.APIKeyID,
		ActionKind: row.ActionKind, Tool: row.Tool, ArgsPretty: pretty, ArgsSHA256: row.ArgsSHA256,
		Expires:   "expires in " + humanizeSince(time.Until(row.ExpiresAt)),
		Age:       humanizeSince(time.Since(row.CreatedAt)) + " ago",
		createdAt: row.CreatedAt,
		KeyLabel:  row.APIKeyID,
	}
	if s.apiKeyRepo != nil && row.APIKeyID != "" {
		if k, err := s.apiKeyRepo.GetByID(ctx, row.APIKeyID); err == nil && k != nil && k.Name != "" {
			card.KeyLabel = k.Name
		}
	}
	card.Grant = s.grantOfferFor(row)
	card.SchemaUnavailable = true
	if s.projectReg != nil {
		if wf := s.projectReg.GetWorkflow(row.WorkflowID); wf != nil && wf.Broker != nil {
			for _, p := range wf.Broker.Proposes {
				if p.Action == row.ActionKind {
					card.UntrustedFields = p.UntrustedArgPaths()
					card.SchemaUnavailable = false
				}
			}
		}
	}
	return card
}

// brokerActionInboxRouter dispatches /inbox/broker-action/{id}/{approve|reject}.
func (s *Server) brokerActionInboxRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/ui"), "/inbox/broker-action/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "approve":
		s.BrokerActionApprove(w, r, parts[0])
	case "reject":
		s.BrokerActionReject(w, r, parts[0])
	case "approve-grant":
		s.BrokerActionApproveWithGrant(w, r, parts[0])
	default:
		http.NotFound(w, r)
	}
}

// BrokerActionApprove approves a pending action. The form carries the
// args_sha256 the card was rendered from; the repository refuses when it no
// longer matches, when the action has expired, or when it is not pending.
// On success the worker is kicked to execute it.
func (s *Server) BrokerActionApprove(w http.ResponseWriter, r *http.Request, actionID string) {
	row, approver, ok := s.brokerActionGate(w, r, actionID)
	if !ok {
		return
	}
	shown := strings.TrimSpace(r.FormValue("args_sha256"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.brokerActionRepo.Approve(ctx, row.ActionID, shown, approver, time.Now().UTC()); err != nil {
		if errors.Is(err, persistence.ErrBrokerActionNoTransition) {
			s.redirectInbox(w, r, "broker-action-not-approvable")
			return
		}
		s.logger.Error().Err(err).Str("action_id", actionID).Msg("broker action approve failed")
		http.Error(w, "approve failed", http.StatusInternalServerError)
		return
	}
	s.logger.Info().Str("action_id", actionID).Str("project", row.ProjectID).Str("approver", approver).
		Msg("broker action approved")
	if s.brokerActionKick != nil {
		s.brokerActionKick(actionID)
	}
	s.signalBrokerActionChanged()
	s.redirectInbox(w, r, "broker-action-approved")
}

// BrokerActionReject declines a pending or approved action.
func (s *Server) BrokerActionReject(w http.ResponseWriter, r *http.Request, actionID string) {
	row, approver, ok := s.brokerActionGate(w, r, actionID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.brokerActionRepo.Reject(ctx, row.ActionID, approver, time.Now().UTC()); err != nil {
		if errors.Is(err, persistence.ErrBrokerActionNoTransition) {
			s.redirectInbox(w, r, "broker-action-already-decided")
			return
		}
		s.logger.Error().Err(err).Str("action_id", actionID).Msg("broker action reject failed")
		http.Error(w, "reject failed", http.StatusInternalServerError)
		return
	}
	s.signalBrokerActionChanged()
	s.redirectInbox(w, r, "broker-action-rejected")
}

// brokerActionGate is the shared approval gate: POST and same origin before
// any read, then project scope and approver once the row is known. Its two
// halves are the only places the checks live; the batch handler uses them
// too (approval fatigue, tier 1).
func (s *Server) brokerActionGate(w http.ResponseWriter, r *http.Request, actionID string) (*persistence.BrokerAction, string, bool) {
	if !s.brokerRequestGate(w, r) {
		return nil, "", false
	}
	row, approver, status, err := s.brokerRowGate(r, actionID)
	if err != nil {
		switch status {
		case http.StatusNotFound:
			http.NotFound(w, r)
		case http.StatusForbidden:
			http.Error(w, "approve this on your approver device", http.StatusForbidden)
		default:
			approval.WriteError(w, r, err)
		}
		return nil, "", false
	}
	return row, approver, true
}

// brokerRequestGate: the store is configured and the request is a
// same-origin POST.
func (s *Server) brokerRequestGate(w http.ResponseWriter, r *http.Request) bool {
	if s.brokerActionRepo == nil {
		http.Error(w, "broker actions not configured", http.StatusServiceUnavailable)
		return false
	}
	if err := approval.CheckRequest(r); err != nil {
		approval.WriteError(w, r, err)
		return false
	}
	return true
}

// brokerRowGate loads the action and authorizes the approver for its
// project. An agent project's action is decided on the approver device only
// (agent-administered Vornik plan P4.8).
func (s *Server) brokerRowGate(r *http.Request, actionID string) (*persistence.BrokerAction, string, int, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := s.brokerActionRepo.Get(ctx, actionID)
	if err != nil || row == nil {
		return nil, "", http.StatusNotFound, errors.New("not found")
	}
	if _, agent := agentns.FromID(row.ProjectID); agent {
		return nil, "", http.StatusForbidden, errors.New("approver device only")
	}
	approver, err := approval.Authorize(r, row.ProjectID, api.RequestAllowsProject, s.operatorIDForRequest)
	if err != nil {
		return nil, "", 0, err
	}
	return row, approver, 0, nil
}

// BrokerActionBatch decides several ticked writes in one POST (approval
// fatigue, tier 1; broker write-actions design, review 5c20). Each pick is
// "<action_id>|<args_sha256 its card showed>" and goes through the same
// per-row checks as a single decision: project scope, approver, and the
// shown-hash binding. A stale or out-of-scope pick is refused and named; the
// others proceed. Nothing is decided without a pick.
func (s *Server) BrokerActionBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.brokerRequestGate(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	approve := r.PostFormValue("decision") == "approve"
	if !approve && r.PostFormValue("decision") != "reject" {
		s.redirectInbox(w, r, "")
		return
	}
	decided := 0
	var refused []string
	for _, pick := range r.PostForm["pick"] {
		id, shown, ok := strings.Cut(pick, "|")
		if !ok || id == "" {
			continue
		}
		if s.decideOneBrokerAction(r, id, strings.TrimSpace(shown), approve) {
			decided++
		} else {
			refused = append(refused, id)
		}
	}
	if decided > 0 {
		s.signalBrokerActionChanged()
	}
	verb := "rejected"
	if approve {
		verb = "approved"
	}
	target := "/ui/inbox?notice=broker-actions-" + verb + "&n=" + strconv.Itoa(decided)
	if len(refused) > 0 {
		target += "&refused=" + url.QueryEscape(strings.Join(refused, ","))
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// decideOneBrokerAction is one pick of a batch: the single-decision checks,
// without writing a response.
func (s *Server) decideOneBrokerAction(r *http.Request, actionID, shown string, approve bool) bool {
	row, approver, _, err := s.brokerRowGate(r, actionID)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if !approve {
		return s.brokerActionRepo.Reject(ctx, row.ActionID, approver, now) == nil
	}
	if err := s.brokerActionRepo.Approve(ctx, row.ActionID, shown, approver, now); err != nil {
		if !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
			s.logger.Error().Err(err).Str("action_id", actionID).Msg("broker action batch approve failed")
		}
		return false
	}
	s.logger.Info().Str("action_id", actionID).Str("project", row.ProjectID).Str("approver", approver).
		Msg("broker action approved (batch)")
	if s.brokerActionKick != nil {
		s.brokerActionKick(actionID)
	}
	return true
}

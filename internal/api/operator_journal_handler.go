package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
)

// The config-apply journal's operator door (config-apply journal design §12,
// GitHub #285):
//
//	GET  /api/v1/operator/journal                 — the open rows
//	POST /api/v1/operator/journal/{id}/resolve    — {"mode":"keep|restore","actor":"..."}
//
// Admin class only (requireAdminClassGate): resolve declares a change applied
// or writes over a hand edit, which no project-scoped or ordinary operator
// key may do. Every resolve that reaches the engine is audited.

// journalResolver is the optional half of the applier the verb needs
// (satisfied by *controlplane.ApplyEngine); an interface so existing applier
// fakes are unaffected.
type journalResolver interface {
	ListOpenJournal(ctx context.Context) ([]*persistence.ConfigApplyJournal, error)
	ResolveJournal(ctx context.Context, id, mode, actor string) (*controlplane.ResolveResult, error)
}

// The production engine is the resolver; a signature drift fails the build,
// not the route at run time.
var _ journalResolver = (*controlplane.ApplyEngine)(nil)

const journalActorMax = 120

type journalRowJSON struct {
	ID               string   `json:"id"`
	ProposalID       string   `json:"proposalId"`
	State            string   `json:"state"`
	AffectedProjects []string `json:"affectedProjects"`
	Failure          string   `json:"failure,omitempty"`
	CreatedAt        string   `json:"createdAt"`
}

type journalResolveResponse struct {
	*controlplane.ResolveResult
	AuditError string `json:"auditError,omitempty"`
}

// OperatorJournal routes the journal door.
func (s *Server) OperatorJournal(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminClassGate(w, r) {
		return
	}
	jr, ok := s.proposalApplier.(journalResolver)
	if !ok || jr == nil {
		respondError(w, http.StatusServiceUnavailable, "JOURNAL_UNAVAILABLE", "the config-apply journal is not wired on this daemon")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/operator/journal"), "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
			return
		}
		s.operatorJournalList(w, r, jr)
		return
	}
	id, ok := strings.CutSuffix(rest, "/resolve")
	if !ok || id == "" || strings.Contains(id, "/") {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "expected /api/v1/operator/journal/{id}/resolve")
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	s.operatorJournalResolve(w, r, jr, id)
}

func (s *Server) operatorJournalList(w http.ResponseWriter, r *http.Request, jr journalResolver) {
	rows, err := jr.ListOpenJournal(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	out := make([]journalRowJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, journalRowJSON{ID: row.ID, ProposalID: row.ProposalID, State: row.State,
			AffectedProjects: row.AffectedProjects, Failure: row.Failure, CreatedAt: row.CreatedAt.UTC().Format(rfc3339)})
	}
	respondJSON(w, http.StatusOK, map[string]any{"rows": out, "count": len(out)})
}

func (s *Server) operatorJournalResolve(w http.ResponseWriter, r *http.Request, jr journalResolver, id string) {
	var body struct {
		Mode  string `json:"mode"`
		Actor string `json:"actor"`
	}
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "body must be JSON: {\"mode\":\"keep|restore\",\"actor\":\"...\"}")
		return
	}
	principal := apiKeyPrincipalFromContext(r.Context())
	if principal == "" {
		principal = "anonymous-admin"
	}
	actor := strings.TrimSpace(body.Actor)
	if len(actor) > journalActorMax || strings.IndexFunc(actor, unicode.IsControl) >= 0 {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "actor must be at most 120 characters with no control characters")
		return
	}
	if actor == "" {
		actor = principal
	}
	res, err := jr.ResolveJournal(r.Context(), id, body.Mode, actor)
	// Every call that reached the engine is audited, a refused one included
	// (a bad mode too: impl review 20261008-461e finding 2).
	auditErr := s.auditJournalResolve(r, principal, id, body.Mode, actor, res, err)
	// Every refusal is a 409 with a remedy, never a 500 (design §12.5.3,
	// GitHub #301). The engine's ResolveRefusal is checked first, so a
	// sentinel it wraps is never misread by a later case.
	var refusal *controlplane.ResolveRefusal
	switch {
	case err == nil:
	case errors.As(err, &refusal):
		respondError(w, http.StatusConflict, refusal.Code, refusal.Error())
		return
	case errors.Is(err, controlplane.ErrResolveMode):
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	case errors.Is(err, persistence.ErrNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "no journal row "+id)
		return
	// The bare sentinels, for an engine that predates ResolveRefusal. The
	// engine's ResolveRefusal.Remedy is the source of truth; these strings
	// are only the fallback (impl review 20261008-8cba I4).
	case errors.Is(err, controlplane.ErrJournalNotDrift):
		respondError(w, http.StatusConflict, controlplane.RefusalNotDrift, err.Error()+
			". Remedy: vornikctl control-plane journal list shows the open rows")
		return
	case errors.Is(err, controlplane.ErrApplyInProgress):
		respondError(w, http.StatusConflict, controlplane.RefusalApplyInProgress, err.Error()+
			". Remedy: retry when the running apply or reconcile finishes")
		return
	case errors.Is(err, controlplane.ErrWriterFenced):
		respondError(w, http.StatusConflict, controlplane.RefusalWriterFenced, err.Error()+
			". Remedy: run the verb against the daemon that holds the config writer lease")
		return
	case errors.Is(err, persistence.ErrProposalNotApproved):
		// The #301 error, should it ever escape the engine unwrapped.
		respondError(w, http.StatusConflict, controlplane.RefusalProposalChanged, err.Error()+
			". Remedy: retry; the next attempt reads the proposal's current status")
		return
	default:
		respondError(w, http.StatusInternalServerError, "RESOLVE_FAILED", err.Error())
		return
	}
	out := journalResolveResponse{ResolveResult: res}
	if auditErr != nil {
		out.AuditError = auditErr.Error()
	}
	respondJSON(w, http.StatusOK, out)
}

// auditJournalResolve writes one admin_audit row for a resolve that reached
// the engine, refused or not. A failure is logged and returned for the
// response; it never undoes the resolution (as the leader-lock release).
func (s *Server) auditJournalResolve(r *http.Request, principal, id, mode, actor string, res *controlplane.ResolveResult, rerr error) error {
	if s.adminAuditRepo == nil {
		return errors.New("no admin audit log is wired")
	}
	after := map[string]any{"mode": mode, "actor": actor}
	if res != nil {
		after["state"], after["proposal_status"], after["changed"] = res.State, res.ProposalStatus, res.Changed
		after["affected_projects"], after["left_in_place"] = res.AffectedProjects, res.LeftInPlace
		if res.ReloadError != "" {
			after["reload_error"] = res.ReloadError
		}
		// What the operator decided and why, as the row records it (#301:
		// "operator kept files of a REJECTED proposal").
		if res.Outcome != "" {
			after["outcome"], after["proposal_status_before"], after["note"] = res.Outcome, res.ProposalStatusBefore, res.Note
		}
	}
	if rerr != nil {
		after["refused"] = rerr.Error()
	}
	raw, _ := json.Marshal(after)
	if err := s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
		Principal: principal, Source: "api", Action: "config_journal.resolve", Target: id,
		After: string(raw), IP: clientIPFromRequest(r), UserAgent: r.UserAgent(),
	}); err != nil {
		s.logger.Warn().Err(err).Str("journal_id", id).Msg("journal resolve: audit write failed; the resolution stands")
		return err
	}
	return nil
}

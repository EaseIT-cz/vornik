package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The account-transition recovery door (design 22 §22.2.16b, GitHub #669
// item 3):
//
//	GET  /api/v1/operator/account-transitions          — the busy rows
//	POST /api/v1/operator/account-transitions/recover  — {"scope","epoch","owner"}
//
// Admin class only (SurfaceAccountTransitions, not granted to tenant_admin):
// recovery clears a busy row, which only the operator who has stopped the
// writers may decide. Every recover call is audited, a refused one included,
// with the row's before-values, as a journal resolve is.

// AccountTransitionView is one busy row as the list shows it. The list
// handler builds the response from it (ageSeconds from Age).
type AccountTransitionView struct {
	Scope     string
	Epoch     int64
	Owner     string
	Operation string
	StartedAt string
	Age       time.Duration
	HeldHere  bool
}

// AccountTransitionRecoverer lists and recovers rows (the service's
// accountRecovery).
type AccountTransitionRecoverer interface {
	ListBusy(ctx context.Context) ([]AccountTransitionView, int, error)
	Recover(ctx context.Context, scope string, epoch int64, owner string) (persistence.AccountTransitionRow, error)
}

// ErrTransitionHeldHere refuses a ticket this process holds: its holder is
// alive, so this is not a recovery.
var ErrTransitionHeldHere = errors.New("this daemon holds that ticket: the transition is running, not abandoned")

// JournalOpenError refuses while open apply-journal rows could touch the
// scope; resolve them first with vornikctl control-plane journal resolve.
type JournalOpenError struct{ IDs []string }

func (e *JournalOpenError) Error() string {
	return fmt.Sprintf("open apply-journal rows could touch this scope (%s); resolve them first: vornikctl control-plane journal resolve <id>", strings.Join(e.IDs, ", "))
}

// WithAccountTransitionRecovery wires the recovery door; nil leaves it 503.
func WithAccountTransitionRecovery(r AccountTransitionRecoverer) ServerOption {
	return func(s *Server) { s.accountRecovery = r }
}

// OperatorAccountTransitions routes the door.
func (s *Server) OperatorAccountTransitions(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperatorScopeFor(w, r, SurfaceAccountTransitions) {
		return
	}
	if s.accountRecovery == nil {
		respondError(w, http.StatusServiceUnavailable, "RECOVERY_UNAVAILABLE", "account-transition recovery is not wired on this daemon")
		return
	}
	switch strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/operator/account-transitions"), "/") {
	case "":
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
			return
		}
		rows, examined, err := s.accountRecovery.ListBusy(r.Context())
		if err != nil {
			s.logger.Warn().Err(err).Msg("account transitions: list failed")
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "the account transitions could not be listed; see the daemon log")
			return
		}
		out := make([]map[string]any, 0, len(rows))
		for _, v := range rows {
			out = append(out, map[string]any{"scope": v.Scope, "epoch": v.Epoch, "owner": v.Owner, "operation": v.Operation,
				"startedAt": v.StartedAt, "ageSeconds": int64(v.Age.Seconds()), "heldHere": v.HeldHere})
		}
		respondJSON(w, http.StatusOK, map[string]any{"rows": out, "count": len(out), "examined": examined})
	case "recover":
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
			return
		}
		s.operatorAccountRecover(w, r)
	default:
		respondError(w, http.StatusNotFound, "NOT_FOUND", "expected /api/v1/operator/account-transitions[/recover]")
	}
}

func (s *Server) operatorAccountRecover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope string `json:"scope"`
		Epoch int64  `json:"epoch"`
		Owner string `json:"owner"`
	}
	limitJSONBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Scope == "" || body.Owner == "" || body.Epoch < 0 {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", `body must be JSON: {"scope":"...","epoch":N,"owner":"<full owner from list>"}`)
		return
	}
	// As journal resolve: an auth-off install has no key principal; the
	// audit row records it as anonymous-admin with the caller's address.
	principal := apiKeyPrincipalFromContext(r.Context())
	if principal == "" {
		principal = "anonymous-admin"
	}
	// The request is audited before anything changes, and a recovery whose
	// request cannot be audited does not run: an epoch never advances with
	// no record of who asked (code review 7546 F2).
	if err := s.auditAccountRecoverRequested(r, principal, body.Scope, body.Epoch, body.Owner); err != nil {
		respondError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "the recovery was not run: its request could not be written to the admin audit log")
		return
	}
	before, err := s.accountRecovery.Recover(r.Context(), body.Scope, body.Epoch, body.Owner)
	auditErr := s.auditAccountRecover(r, principal, body.Scope, body.Epoch, body.Owner, before, err)
	var jo *JournalOpenError
	switch {
	case err == nil:
		resp := map[string]any{"recovered": true, "scope": body.Scope, "epochBefore": before.Epoch, "epochAfter": before.Epoch + 1,
			"operation": before.Operation, "startedAt": before.StartedAt,
			"note": "suspended grants stay suspended; confirm them on the approver device"}
		if auditErr != nil {
			resp["auditError"] = auditErr.Error()
		}
		respondJSON(w, http.StatusOK, resp)
	case errors.Is(err, ErrTransitionHeldHere):
		respondError(w, http.StatusConflict, "TRANSITION_HELD", err.Error())
	case errors.As(err, &jo):
		respondError(w, http.StatusConflict, "JOURNAL_OPEN", err.Error())
	case errors.Is(err, persistence.ErrAccountTransition):
		respondError(w, http.StatusConflict, "NO_MATCH", "no busy row matches that scope, epoch and owner; list again (it may have finished or been recovered)")
	default:
		s.logger.Warn().Err(err).Str("scope", body.Scope).Msg("account transitions: recover failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "the recovery failed; see the daemon log")
	}
}

// auditAccountRecoverRequested records who asked, before the CAS runs.
func (s *Server) auditAccountRecoverRequested(r *http.Request, principal, scope string, epoch int64, owner string) error {
	if s.adminAuditRepo == nil {
		return errors.New("no admin audit log is wired")
	}
	raw, _ := json.Marshal(map[string]any{"scope": scope, "epoch_requested": epoch, "owner": owner})
	return s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
		Principal: principal, Source: "api", Action: "account_transition.recover_requested", Target: scope,
		After: string(raw), IP: clientIPFromRequest(r), UserAgent: r.UserAgent(),
	})
}

// auditAccountRecover records the call: the before-values (the row as the
// CAS matched it), the epoch after, the actor, and a refusal's reason.
func (s *Server) auditAccountRecover(r *http.Request, principal, scope string, epoch int64, owner string, before persistence.AccountTransitionRow, rerr error) error {
	if s.adminAuditRepo == nil {
		return errors.New("no admin audit log is wired")
	}
	after := map[string]any{"scope": scope, "epoch_requested": epoch, "owner": owner}
	if rerr == nil {
		after["epoch_before"], after["epoch_after"] = before.Epoch, before.Epoch+1
		after["operation"], after["started_at"] = before.Operation, before.StartedAt
		if t, err := time.Parse(time.RFC3339Nano, before.StartedAt); err == nil {
			after["age_seconds"] = int64(time.Since(t).Seconds())
		}
	} else {
		after["refused"] = rerr.Error()
	}
	raw, _ := json.Marshal(after)
	// The outcome is written even if the caller has gone: the CAS already
	// ran (code review 7c7a F5; the drain audit's WithoutCancel precedent).
	if err := s.adminAuditRepo.Insert(context.WithoutCancel(r.Context()), &persistence.AdminAuditEntry{
		Principal: principal, Source: "api", Action: "account_transition.recover", Target: scope,
		After: string(raw), IP: clientIPFromRequest(r), UserAgent: r.UserAgent(),
	}); err != nil {
		s.logger.Warn().Err(err).Str("scope", scope).Msg("account recovery: audit write failed; the outcome stands")
		return err
	}
	return nil
}

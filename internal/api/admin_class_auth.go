package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/budget"
)

// isAdminClassRequest reports whether the caller holds GENERAL admin-class
// authority — an admin API key OR an admin browser session — as opposed to a
// plain project-scoped key. Unlike requireAdminGate this is NOT gated on the EE
// control-plane admin surface (adminSurfacePresent / 501 EDITION_UNSUPPORTED),
// so it works in BOTH editions: it's a pure role check, used by the per-task
// cost governor's "increase budget & resume" branch (LLD 2026-07-24 §3.6, impl
// review I5).
//
// When auth is disabled the trusted local operator passes (mirrors
// requireAdminGate's first line). A project-scoped key never passes — that
// preserves §3.1's no-self-serving-budget rule (a project key can't raise its
// own task's ceiling).
func (s *Server) isAdminClassRequest(r *http.Request) bool {
	return IsAdminClassRequest(r, s.isAdminClassKey)
}

// IsAdminClassRequest is the one admin-class predicate behind
// Server.isAdminClassRequest, exported so the ui package's Fix-It panel gates
// on the same rule instead of a second implementation (doctor design §E8.8):
// auth off, an admin browser session, or a presented key isAdminKey accepts
// (the operator's admin.allowed_keys). Being UNSCOPED is not enough: a plain
// static api.api_keys key is unscoped and is not admin-class. A nil
// isAdminKey accepts no key.
func IsAdminClassRequest(r *http.Request, isAdminKey func(string) bool) bool {
	if !IsAuthEnabledFromContext(r.Context()) {
		return true
	}
	if SessionRoleFromContext(r.Context()) == "admin" {
		return true
	}
	key := APIKeyFromContext(r.Context())
	return key != "" && isAdminKey != nil && isAdminKey(key)
}

// ErrorTextAccess is what a caller may read of a task's logs, explain, live
// stream and persisted error text (GitHub #440, #466, #491; process-spawn-law
// design, "Amendment 2026-10-21 (#491)", K1). The zero value is
// ErrorTextDenied, so an unset answer refuses.
type ErrorTextAccess int

const (
	// ErrorTextDenied reads nothing: an unscoped caller that is not
	// admin-class (operator decision B7: deny until scoped).
	ErrorTextDenied ErrorTextAccess = iota
	// ErrorTextScrubbed reads the scrubbed view (#440, #466).
	ErrorTextScrubbed
	// ErrorTextOperator reads the operator view, bytes unchanged.
	ErrorTextOperator
)

// UnscopedWithheldText replaces, whole, a persisted error field on a
// structured read for an ErrorTextDenied caller (#491, K2): no line of the
// text, raw or scrubbed, reaches it.
const UnscopedWithheldText = "[withheld: key not scoped to a project]"

// ErrorTextAccessFor is the one decision behind every reader of the task
// logs, explain, live stream and persisted error text, in this order: an
// admin-class caller (IsAdminClassRequest) reads the operator view; a session
// (whatever its grant: a ["*"] group grant is the operator's own scoping act,
// a no-access session carries the sentinel scope the project gate refuses)
// and a request carrying a project scope (every DB key, a scoped static key)
// read the scrubbed view; everything else is denied, so a principal a later
// backend adds without a scope is refused until it is placed.
func ErrorTextAccessFor(r *http.Request, isAdminKey func(string) bool) ErrorTextAccess {
	if IsAdminClassRequest(r, isAdminKey) {
		return ErrorTextOperator
	}
	if id := IdentityFromContext(r.Context()); id != nil && id.Backend == "session" {
		return ErrorTextScrubbed
	}
	if _, scoped := requestScopedProjectSet(r); scoped {
		return ErrorTextScrubbed
	}
	return ErrorTextDenied
}

// OperatorSurfaceAccess is what a caller gets of the operator surface: the
// daemon-level routes behind requireOperatorScope (GitHub #495;
// 18-security-and-tenancy.md, "Amendment 2026-10-21 (#495)", O1). The zero
// value is OperatorSurfaceRefused, so an unset answer refuses.
type OperatorSurfaceAccess int

const (
	// OperatorSurfaceRefused answers 403: an unscoped caller that is not
	// admin-class, and any principal a later backend adds without a place.
	OperatorSurfaceRefused OperatorSurfaceAccess = iota
	// OperatorSurfaceHidden answers 404: a project-scoped tenant, which is
	// not told the surface exists.
	OperatorSurfaceHidden
	// OperatorSurfaceAllowed passes: an admin-class caller.
	OperatorSurfaceAllowed
)

// OperatorSurfaceAccessFor is the one decision behind every operator-surface
// route, in this order: an admin-class caller (IsAdminClassRequest: auth off,
// an admin session whatever its grant, an admin key) is allowed; a request
// carrying a project scope is hidden; everything else is refused. Being
// unscoped is not an operator capability: a plain api.api_keys static key, a
// ["*"] user session, an empty-project DB key and an hmac-webhook identity
// are all unscoped and all refused.
func OperatorSurfaceAccessFor(r *http.Request, isAdminKey func(string) bool) OperatorSurfaceAccess {
	if IsAdminClassRequest(r, isAdminKey) {
		return OperatorSurfaceAllowed
	}
	if _, scoped := requestScopedProjectSet(r); scoped {
		return OperatorSurfaceHidden
	}
	return OperatorSurfaceRefused
}

// operatorSurfaceAccess is OperatorSurfaceAccessFor over the server's
// admin-key checker.
func (s *Server) operatorSurfaceAccess(r *http.Request) OperatorSurfaceAccess {
	return OperatorSurfaceAccessFor(r, s.isAdminClassKey)
}

// errorTextAccess is ErrorTextAccessFor over the server's admin-key checker.
func (s *Server) errorTextAccess(r *http.Request) ErrorTextAccess {
	return ErrorTextAccessFor(r, s.isAdminClassKey)
}

// WithheldUnscoped is the ErrorTextDenied view of one persisted error field:
// the constant for any text, an empty text left empty (so no error block is
// invented where there was none).
func WithheldUnscoped(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	return UnscopedWithheldText
}

// RespondProjectScopeRequired is the ErrorTextDenied answer of a route whose
// body is the log, the explain or a stream of either (#491, K2).
func RespondProjectScopeRequired(w http.ResponseWriter) {
	respondError(w, http.StatusForbidden, "PROJECT_SCOPE_REQUIRED",
		"the task logs, explain and error text need a project-scoped key or an admin-class key")
}

// requireAdminClass gates a doctor route on isAdminClassRequest and writes the
// refusal. Unlike requireAdminGate it answers the same in both editions: the
// doctor ships in Community, where the admin surface answers 501.
//
// Auth off passes (the trusted local operator). Auth on with no server wired
// fails CLOSED with 503: there is nothing to check the caller against.
func (h *DoctorHandlers) requireAdminClass(w http.ResponseWriter, r *http.Request) bool {
	if !IsAuthEnabledFromContext(r.Context()) {
		return true
	}
	if h.server == nil {
		respondError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "server not wired; cannot authenticate")
		return false
	}
	if !h.server.isAdminClassRequest(r) {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED",
			"the doctor needs an admin-class key (admin.allowed_keys, with admin.enabled) or an admin session")
		return false
	}
	return true
}

// IsBudgetCheckpoint reports whether a checkpoint message's metadata is a
// per-task cost-governor budget decision (decision.kind == "budget"), so the
// answer handlers (API + UI) can branch onto the resume-with-optional-top-up
// path. Exported so the ui package's uiAnswerCheckpoint shares one definition.
func IsBudgetCheckpoint(meta []byte) bool {
	if len(meta) == 0 {
		return false
	}
	var m struct {
		Decision struct {
			Kind string `json:"kind"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return false
	}
	return m.Decision.Kind == "budget"
}

// clampTaskBudgetForProject applies the project's optional MaxTaskBudgetUSD
// ceiling to a requested per-task budget, returning the (possibly clamped)
// value and whether the clamp actually reduced it. Nil-safe on registry/project.
func (s *Server) clampTaskBudgetForProject(projectID string, requested float64) (float64, bool) {
	if s.projectRegistry == nil {
		return requested, false
	}
	proj := s.projectRegistry.GetProject(projectID)
	if proj == nil {
		return requested, false
	}
	clamped := budget.ClampTaskBudget(proj, requested)
	return clamped, clamped < requested
}

// budgetFromAnswerMetadata extracts the operator-supplied new per-task budget
// (budget_usd) from a checkpoint-answer's metadata blob. Returns (value, true)
// only when a numeric budget_usd is present.
func budgetFromAnswerMetadata(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false
	}
	v, ok := m["budget_usd"]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

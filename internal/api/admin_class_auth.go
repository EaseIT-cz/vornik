package api

import (
	"encoding/json"
	"net/http"

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

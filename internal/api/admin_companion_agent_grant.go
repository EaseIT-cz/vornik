package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// WithApproverDevices gives the grant the approver-device store, so minting
// an agent admin key can refuse while no device exists (design §11).
func WithApproverDevices(repo persistence.ApproverDeviceRepository) ServerOption {
	return func(srv *Server) { srv.approverDevices = repo }
}

// companionGrantAgentAdmin mints the agent admin key of a namespace
// (agent-administered Vornik design §5, §11; plan P3.1). Refusals, in order:
// not an operator; auth off; a malformed namespace; capabilities an agent
// admin key may not carry; no active approver device (a key must never exist
// before the user can approve on a phone); a namespace that already has a
// live key (checked here, and enforced by the database for concurrent grants). The key
// is bound to the namespace's home project, which this creates through the
// same inert path the verbs use.
func (s *Server) companionGrantAgentAdmin(w http.ResponseWriter, r *http.Request, req *companionGrantRequest) {
	if !s.requireAdminClassGate(w, r) {
		return
	}
	if !IsAuthEnabledFromContext(r.Context()) {
		respondError(w, http.StatusConflict, "AUTH_REQUIRED", "an agent admin key needs API authentication turned on")
		return
	}
	ns := strings.TrimSpace(req.Namespace)
	if !agentns.Valid(ns) {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "namespace must be 2-16 characters of a-z and 0-9, starting with a letter")
		return
	}
	if s.agentAdmin == nil || s.approverDevices == nil {
		respondError(w, http.StatusServiceUnavailable, "AGENT_ADMIN_UNAVAILABLE", "the agent admin service is not wired into this daemon")
		return
	}
	if req.SkillAdmin || req.SkillWrite || req.MemoryWrite || req.AllowedWorkflows != nil || req.DelegateDisabled {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "an agent admin key carries no skill, memory-write, workflow-list or no-delegate settings")
		return
	}
	n, err := s.approverDevices.CountActiveDevices(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "could not count approver devices")
		return
	}
	if n == 0 {
		respondError(w, http.StatusConflict, "NO_APPROVER_DEVICE", "pair an approver device first (vornikctl pair-device): an agent key must never exist before you can approve on your phone")
		return
	}
	home := agentns.ID(ns, "home")
	existing, err := s.apiKeyRepo.ListByProject(r.Context(), home)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "could not list keys")
		return
	}
	for _, k := range existing {
		if k.AgentAdmin && k.RevokedAt == nil {
			respondError(w, http.StatusConflict, "NAMESPACE_TAKEN", "namespace "+ns+" already has an agent admin key ("+k.ID+"); revoke it first")
			return
		}
	}
	if _, err := s.agentAdmin.EnsureHome(r.Context(), ns, req.ClientKind); err != nil {
		if errors.Is(err, agentadmin.ErrUnavailable) {
			respondError(w, http.StatusServiceUnavailable, "AGENT_ADMIN_UNAVAILABLE", err.Error())
			return
		}
		respondError(w, http.StatusConflict, "HOME_PROJECT", err.Error())
		return
	}
	secret, err := apikey.Generate(home)
	if err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_PROJECT", err.Error())
		return
	}
	now := time.Now().UTC()
	name := req.SessionLabel
	if name == "" {
		name = "agent-admin-" + ns
	}
	row := &persistence.APIKey{
		ID: persistence.GenerateID("akey"), ProjectID: home, Name: name,
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: now, ExpiresAt: req.ExpiresAt, CreatedBy: callerForAudit(r),
		BudgetCapUSD: req.BudgetCapUSD, ClientKind: req.ClientKind, SessionLabel: req.SessionLabel,
		DefaultRepoScope: req.DefaultRepoScope, AgentAdmin: true, AgentNamespace: ns,
	}
	if err := s.apiKeyRepo.Create(r.Context(), row); err != nil {
		if errors.Is(err, persistence.ErrDuplicateKey) {
			// A concurrent grant won the namespace (the database holds the
			// one-live-key rule, migration 210).
			respondError(w, http.StatusConflict, "NAMESPACE_TAKEN", "namespace "+ns+" already has an agent admin key")
			return
		}
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to create the agent admin key")
		return
	}
	respondJSON(w, http.StatusCreated, companionGrantResponse{
		ID: row.ID, ProjectID: row.ProjectID, SessionLabel: row.SessionLabel, ClientKind: row.ClientKind,
		Secret: secret, KeyPrefix: row.KeyPrefix, BudgetCapUSD: row.BudgetCapUSD, CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt, DefaultRepoScope: row.DefaultRepoScope, AgentAdmin: true, Namespace: ns,
	})
}

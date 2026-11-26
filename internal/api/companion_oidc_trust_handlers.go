package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #792 (design 27 §C4): the operator's surface for trusts. Same gate
// as CompanionGrant (the caller must be allowed the project); a companion key
// cannot reach it (isCompanionAllowedPath).
//
//	GET    /api/v1/companion/trusts?projectId=P
//	POST   /api/v1/companion/trusts            {projectId, keyId, repository, repositoryId, ref, workflow, events, audience}
//	DELETE /api/v1/companion/trusts?projectId=P&id=ktrust_…

var (
	trustRepoRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	trustRefRe      = regexp.MustCompile(`^refs/(heads|tags)/[A-Za-z0-9_./-]+$`)
	trustWorkflowRe = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$`)
	// The events a trust may name. pull_request is refused: its ref is
	// refs/pull/<n>/merge, which never matches a branch.
	trustEvents = map[string]bool{"push": true, "workflow_dispatch": true, "schedule": true, "pull_request_target": true}
)

// DefaultTrustEvents applies when a trust names none.
var DefaultTrustEvents = []string{"push", "schedule", "workflow_dispatch"}

type companionTrustRequest struct {
	ProjectID    string   `json:"projectId"`
	KeyID        string   `json:"keyId"`
	Repository   string   `json:"repository"`
	RepositoryID int64    `json:"repositoryId"`
	Ref          string   `json:"ref"`
	Workflow     string   `json:"workflow"`
	Events       []string `json:"events"`
	Audience     string   `json:"audience"`
}

type companionTrustEntry struct {
	ID             string     `json:"id"`
	KeyID          string     `json:"keyId"`
	Repository     string     `json:"repository"`
	RepositoryID   int64      `json:"repositoryId"`
	Ref            string     `json:"ref"`
	JobWorkflowRef string     `json:"jobWorkflowRef"`
	Events         []string   `json:"events"`
	Audience       string     `json:"audience"`
	CreatedAt      time.Time  `json:"createdAt"`
	CreatedBy      string     `json:"createdBy"`
	LastUsedAt     *time.Time `json:"lastUsedAt"`
}

// CompanionTrusts serves the trust surface.
func (s *Server) CompanionTrusts(w http.ResponseWriter, r *http.Request) {
	if s.oidcTrusts == nil || s.apiKeyRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "TRUSTS_DISABLED", "the trust store is not configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listCompanionTrusts(w, r)
	case http.MethodPost:
		s.createCompanionTrust(w, r)
	case http.MethodDelete:
		s.deleteCompanionTrust(w, r)
	default:
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET, POST or DELETE")
	}
}

func (s *Server) trustProject(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if strings.TrimSpace(projectID) == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return false
	}
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found: "+projectID)
		return false
	}
	return true
}

func (s *Server) listCompanionTrusts(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if !s.trustProject(w, r, projectID) {
		return
	}
	rows, err := s.oidcTrusts.OIDCTrustsForProject(r.Context(), projectID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to list trusts")
		return
	}
	out := make([]companionTrustEntry, 0, len(rows))
	for _, t := range rows {
		out = append(out, trustEntry(t))
	}
	respondJSON(w, http.StatusOK, map[string]any{"trusts": out})
}

func (s *Server) createCompanionTrust(w http.ResponseWriter, r *http.Request) {
	var req companionTrustRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}
	if !s.trustProject(w, r, req.ProjectID) {
		return
	}
	if msg := validateTrustRequest(&req); msg != "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	key, err := s.apiKeyRepo.GetByID(r.Context(), req.KeyID)
	if err != nil || key == nil || key.ProjectID != req.ProjectID {
		respondError(w, http.StatusNotFound, "KEY_NOT_FOUND", "no such key in this project: "+req.KeyID)
		return
	}
	if key.ClientKind == "" {
		respondError(w, http.StatusBadRequest, "NOT_A_COMPANION_KEY", "a trust binds only a companion key")
		return
	}
	if key.RevokedAt != nil {
		respondError(w, http.StatusBadRequest, "KEY_REVOKED", "the key is revoked")
		return
	}
	t := persistence.CompanionOIDCTrust{
		ID: "ktrust_" + hexID(24), KeyID: key.ID, Issuer: auth.GitHubActionsIssuer,
		Audience: req.Audience, RepositoryID: req.RepositoryID, Repository: req.Repository, Ref: req.Ref,
		JobWorkflowRef: req.Repository + "/" + req.Workflow + "@" + req.Ref, EventNames: req.Events,
		CreatedAt: time.Now().UTC(), CreatedBy: trustCreator(r),
	}
	// One workflow is served as one key: refuse a trust whose events overlap
	// an existing one with the same other claims (review 3e53 F1). The
	// backend also refuses a token two trusts match.
	existing, err := s.oidcTrusts.OIDCTrustsForRepository(r.Context(), t.Issuer, t.RepositoryID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to read trusts")
		return
	}
	for _, e := range existing {
		if persistence.OIDCTrustsOverlap(e, t) {
			respondError(w, http.StatusConflict, "TRUST_DUPLICATE",
				"trust "+e.ID+" already serves these claims for an event in this list: one workflow is served as one key, so remove it first")
			return
		}
	}
	if err := s.oidcTrusts.CreateOIDCTrust(r.Context(), t); err != nil {
		if errors.Is(err, persistence.ErrOIDCTrustDuplicate) {
			respondError(w, http.StatusConflict, "TRUST_DUPLICATE",
				"a trust with these claims exists: one workflow is served as one key, so remove it first")
			return
		}
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to create the trust")
		return
	}
	t.EventNames = persistence.DecodeOIDCEventNames(persistence.EncodeOIDCEventNames(t.EventNames))
	respondJSON(w, http.StatusCreated, trustEntry(t))
}

func (s *Server) deleteCompanionTrust(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if !s.trustProject(w, r, projectID) {
		return
	}
	ok, err := s.oidcTrusts.DeleteOIDCTrust(r.Context(), projectID, strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to remove the trust")
		return
	}
	if !ok {
		respondError(w, http.StatusNotFound, "TRUST_NOT_FOUND", "no such trust in this project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateTrustRequest normalises req and returns a refusal, or "".
func validateTrustRequest(req *companionTrustRequest) string {
	req.Repository = strings.TrimSpace(req.Repository)
	req.Ref = strings.TrimSpace(req.Ref)
	req.Workflow = strings.TrimPrefix(strings.TrimSpace(req.Workflow), "./")
	req.Audience = strings.TrimSpace(req.Audience)
	switch {
	case req.KeyID == "":
		return "keyId is required"
	case !trustRepoRe.MatchString(req.Repository):
		return "repository must be owner/name"
	case req.RepositoryID <= 0:
		return "repositoryId must be the repository's numeric id (gh api repos/<owner>/<name> --jq .id)"
	case !trustRefRe.MatchString(req.Ref):
		return "ref must be refs/heads/<branch> or refs/tags/<tag>"
	case !trustWorkflowRe.MatchString(req.Workflow):
		return "workflow must be .github/workflows/<file>.yml"
	case !strings.HasPrefix(req.Audience, "https://"):
		return "audience must be the https:// URL the job requests its token for"
	}
	if len(req.Events) == 0 {
		req.Events = DefaultTrustEvents
	}
	for _, e := range req.Events {
		if e == "pull_request" {
			return "pull_request cannot be trusted: its ref is refs/pull/<n>/merge, never a branch"
		}
		if !trustEvents[e] {
			return "event not allowed: " + e
		}
	}
	return ""
}

func trustEntry(t persistence.CompanionOIDCTrust) companionTrustEntry {
	return companionTrustEntry{ID: t.ID, KeyID: t.KeyID, Repository: t.Repository, RepositoryID: t.RepositoryID,
		Ref: t.Ref, JobWorkflowRef: t.JobWorkflowRef, Events: t.EventNames, Audience: t.Audience,
		CreatedAt: t.CreatedAt, CreatedBy: t.CreatedBy, LastUsedAt: t.LastUsedAt}
}

// trustCreator names who created a trust, for the audit column: the caller's
// identity subject, a key id for a key caller or a user id for a session.
func trustCreator(r *http.Request) string {
	if id := IdentityFromContext(r.Context()); id != nil && id.Subject != "" {
		return id.Subject
	}
	return "unknown"
}

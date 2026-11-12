package api

// The REST verb for operator credentials (GitHub #625 item 2; agent-administered
// design, amendment #623, follow-up 2026-11-11): list, set and remove an
// operator credential with an admin-class API key, through the same service the
// console page uses, which audits each write. The value is the request body,
// read once and never logged, echoed or put in an error.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
)

// maxOperatorCredentialBytes caps a value, as the console page does.
const maxOperatorCredentialBytes = 16 << 10

// operatorCredentialRefreshEvery bounds the MCP re-registration a write
// triggers, so a rotation script cannot turn into a re-dial storm.
const operatorCredentialRefreshEvery = 5 * time.Second

// operatorCredentialRefreshTimeout bounds one refresh (code review 0bdf F2).
const operatorCredentialRefreshTimeout = 60 * time.Second

// OperatorCredentialInfo is one name, never its value.
type OperatorCredentialInfo struct {
	Name     string     `json:"name"`
	State    string     `json:"state"` // stored, environment, both, missing
	SetAt    *time.Time `json:"set_at,omitempty"`
	SetBy    string     `json:"set_by,omitempty"`
	NeededBy []string   `json:"needed_by,omitempty"`
}

// OperatorCredentialList is the GET answer.
type OperatorCredentialList struct {
	KeyUnusable bool                     `json:"key_unusable,omitempty"`
	Credentials []OperatorCredentialInfo `json:"credentials"`
}

// OperatorCredentialsRefresh is what a refresh re-registered.
type OperatorCredentialsRefresh struct {
	DaemonPresent []string `json:"daemon_present,omitempty"`
	ProjectDials  []string `json:"project_dials,omitempty"`
}

// OperatorCredentialsService is the service the verb calls.
type OperatorCredentialsService interface {
	List(ctx context.Context) (OperatorCredentialList, error)
	Put(ctx context.Context, name string, value []byte, who persistence.AdminAuditEntry) error
	// Delete reports whether a stored value was removed.
	Delete(ctx context.Context, name string, who persistence.AdminAuditEntry) (bool, error)
	Refresh(ctx context.Context) (OperatorCredentialsRefresh, error)
}

// WithOperatorCredentialsService wires the verb.
func WithOperatorCredentialsService(svc OperatorCredentialsService) ServerOption {
	return func(s *Server) { s.operatorCreds = svc }
}

// opCredRefresher bounds refreshes to one per window; a write inside the
// window arms one deferred refresh for the window's end.
type opCredRefresher struct {
	mu    sync.Mutex
	last  time.Time
	armed bool
}

// AdminOperatorCredentials serves /api/v1/admin/operator-credentials and
// /api/v1/admin/operator-credentials/{name}.
func (s *Server) AdminOperatorCredentials(w http.ResponseWriter, r *http.Request) {
	// requireAdminGate admits a request when API auth is off; a credential
	// write must never take that path, so it is refused here first.
	if !IsAuthEnabledFromContext(r.Context()) {
		respondError(w, http.StatusForbidden, "AUTH_REQUIRED", "operator credentials need API authentication and an admin-class key")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.operatorCreds == nil {
		respondError(w, http.StatusServiceUnavailable, "CREDENTIALS_UNAVAILABLE", "operator credentials are not available on this daemon")
		return
	}
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/operator-credentials"), "/")
	who := persistence.AdminAuditEntry{Principal: RequestPrincipal(r.Context()), Source: "api",
		IP: clientIPFromRequest(r), UserAgent: r.UserAgent()}
	switch {
	case name == "" && r.Method == http.MethodGet:
		list, err := s.operatorCreds.List(r.Context())
		if err != nil {
			respondError(w, http.StatusInternalServerError, "CREDENTIALS_LIST_FAILED", "the credentials could not be listed")
			return
		}
		respondJSON(w, http.StatusOK, list)
	case name != "" && r.Method == http.MethodPut:
		if !agentns.ValidSecretName(name) {
			respondError(w, http.StatusBadRequest, "INVALID_NAME", "names must be UPPER_SNAKE_CASE: A to Z, 0 to 9 and _, starting with a letter, at most 64 characters")
			return
		}
		value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOperatorCredentialBytes))
		if err != nil {
			respondError(w, http.StatusRequestEntityTooLarge, "VALUE_TOO_LARGE", "a value is at most 16 KiB")
			return
		}
		if len(value) == 0 {
			respondError(w, http.StatusBadRequest, "EMPTY_VALUE", "the request body is the value; it is empty")
			return
		}
		if err := s.operatorCreds.Put(r.Context(), name, value, who); err != nil {
			s.logger.Warn().Str("credential", name).Msg("operator credentials API: store failed")
			respondError(w, http.StatusInternalServerError, "STORE_FAILED", "the credential could not be stored")
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"name": name, "stored": true, "refresh": s.boundedCredentialRefresh()})
	case name != "" && r.Method == http.MethodDelete:
		if !agentns.ValidSecretName(name) {
			respondError(w, http.StatusBadRequest, "INVALID_NAME", "names must be UPPER_SNAKE_CASE: A to Z, 0 to 9 and _, starting with a letter, at most 64 characters")
			return
		}
		removed, err := s.operatorCreds.Delete(r.Context(), name, who)
		if err != nil {
			s.logger.Warn().Str("credential", name).Msg("operator credentials API: remove failed")
			respondError(w, http.StatusInternalServerError, "REMOVE_FAILED", "the credential could not be removed")
			return
		}
		resp := map[string]any{"name": name, "removed": removed}
		if removed {
			resp["refresh"] = s.boundedCredentialRefresh()
		}
		respondJSON(w, http.StatusOK, resp)
	default:
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET the collection; PUT or DELETE a name")
	}
}

// boundedCredentialRefresh runs the refresh now when the window has passed,
// else arms one deferred refresh and answers "deferred".
func (s *Server) boundedCredentialRefresh() any {
	rf := &s.opCredRefresh
	rf.mu.Lock()
	wait := operatorCredentialRefreshEvery - time.Since(rf.last)
	if wait > 0 {
		if !rf.armed {
			rf.armed = true
			time.AfterFunc(wait, func() {
				rf.mu.Lock()
				rf.armed, rf.last = false, time.Now()
				rf.mu.Unlock()
				svc := s.operatorCreds
				if svc == nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), operatorCredentialRefreshTimeout)
				defer cancel()
				if _, err := svc.Refresh(ctx); err != nil {
					s.logger.Warn().Err(err).Msg("operator credentials API: deferred refresh failed")
				}
			})
		}
		rf.mu.Unlock()
		return "deferred"
	}
	rf.last = time.Now()
	rf.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), operatorCredentialRefreshTimeout)
	defer cancel()
	rep, err := s.operatorCreds.Refresh(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("operator credentials API: refresh failed")
		return "failed: a config reload will register the servers"
	}
	return rep
}

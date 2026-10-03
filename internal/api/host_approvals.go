package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// The Hermes approval transport's REST pair
// (https://docs.vornik.io §4.3).
// Not a companion MCP tool: the bridge relays every tool to the model, so
// the agent would see a tool that pings the user's phone, and every turn
// would pay for its schema. `vornikctl agent host-approval` is the caller.

const (
	hostApprovalsPath   = "/api/v1/agent/host-approvals"
	hostApprovalsPrefix = hostApprovalsPath + "/"
	// hostApprovalMaxWait bounds one long-poll call.
	hostApprovalMaxWait = 25 * time.Second
	// hostApprovalPoll is how often a waiting GET re-reads the row.
	hostApprovalPoll = 500 * time.Millisecond
	// hostApprovalMaxBody bounds a filing; the command is cut at 4 KiB
	// after it arrives, so this only stops an abusive body.
	hostApprovalMaxBody = 256 << 10
)

// HostApprovals files and reads host-action requests (approverdevice.Service).
type HostApprovals interface {
	FileHostAction(ctx context.Context, ns string, req approverdevice.HostActionRequest) (approverdevice.HostActionState, error)
	HostActionState(ctx context.Context, ns, requestID string) (approverdevice.HostActionState, error)
}

// WithHostApprovals wires the host-approval routes.
func WithHostApprovals(h HostApprovals) ServerOption {
	return func(srv *Server) { srv.hostApprovals = h }
}

// isHostApprovalPath is the exact shape of the two routes: the collection,
// or one segment under it.
func isHostApprovalPath(p string) bool {
	if p == hostApprovalsPath {
		return true
	}
	rest, ok := strings.CutPrefix(p, hostApprovalsPrefix)
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// isCompanionAllowedPathFor is the companion-key allowlist for one key: the
// companion surface for every companion key, plus the host-approval routes
// for an agent admin key only (design §4.3).
func isCompanionAllowedPathFor(key *persistence.APIKey, p string) bool {
	if isCompanionAllowedPath(p) {
		return true
	}
	return key != nil && key.AgentAdmin && isHostApprovalPath(p)
}

// hostApprovalWait reads ?wait= as seconds, clamped to [0, 25].
func hostApprovalWait(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	if d := time.Duration(n) * time.Second; d < hostApprovalMaxWait {
		return d
	}
	return hostApprovalMaxWait
}

type hostApprovalResponse struct {
	Status   string    `json:"status"`
	Choice   string    `json:"choice,omitempty"`
	Deadline time.Time `json:"deadline"`
}

func writeHostApproval(w http.ResponseWriter, st approverdevice.HostActionState) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(hostApprovalResponse{Status: st.Status, Choice: st.Choice, Deadline: st.Deadline.UTC()})
}

func hostApprovalError(w http.ResponseWriter, status int, code, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "reason": reason})
}

// hostApprovalsRouter serves both routes. Only an agent admin key reaches
// it past the middleware; it checks again here, because an operator key
// (no DB row) or a web session passes the middleware's companion guard.
func (s *Server) hostApprovalsRouter(w http.ResponseWriter, r *http.Request) {
	key, err := s.resolveCompanionKey(r)
	if err != nil || key == nil || !key.AgentAdmin || key.AgentNamespace == "" {
		hostApprovalError(w, http.StatusForbidden, "forbidden", "only an agent admin key may file or read host approvals")
		return
	}
	if s.hostApprovals == nil {
		hostApprovalError(w, http.StatusServiceUnavailable, "unavailable", "host approvals are not wired on this daemon")
		return
	}
	ns := key.AgentNamespace
	switch {
	case r.URL.Path == hostApprovalsPath && r.Method == http.MethodPost:
		s.fileHostApproval(w, r, ns)
	case isHostApprovalPath(r.URL.Path) && r.URL.Path != hostApprovalsPath && r.Method == http.MethodGet:
		s.pollHostApproval(w, r, ns, strings.TrimPrefix(r.URL.Path, hostApprovalsPrefix))
	default:
		hostApprovalError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST "+hostApprovalsPath+" or GET "+hostApprovalsPrefix+"{request_id}")
	}
}

func (s *Server) fileHostApproval(w http.ResponseWriter, r *http.Request, ns string) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hostApprovalMaxBody))
	if err != nil {
		hostApprovalError(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large")
		return
	}
	var req approverdevice.HostActionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		hostApprovalError(w, http.StatusBadRequest, "invalid", "the body is not a host approval request")
		return
	}
	st, err := s.hostApprovals.FileHostAction(r.Context(), ns, req)
	switch {
	case err == nil:
		writeHostApproval(w, st)
	case errors.Is(err, approverdevice.ErrHostActionInvalid):
		hostApprovalError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, approverdevice.ErrHostActionConflict):
		hostApprovalError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, approverdevice.ErrHostActionBusy):
		hostApprovalError(w, http.StatusTooManyRequests, "busy", err.Error())
	default:
		s.logger.Warn().Err(err).Str("namespace", ns).Msg("host approval: filing failed")
		hostApprovalError(w, http.StatusInternalServerError, "error", "the request could not be filed")
	}
}

func (s *Server) pollHostApproval(w http.ResponseWriter, r *http.Request, ns, requestID string) {
	wait := hostApprovalWait(r.URL.Query().Get("wait"))
	deadline := time.Now().Add(wait)
	for {
		st, err := s.hostApprovals.HostActionState(r.Context(), ns, requestID)
		switch {
		case errors.Is(err, persistence.ErrNotFound):
			hostApprovalError(w, http.StatusNotFound, "not_found", "no such host approval request")
			return
		case err != nil:
			s.logger.Warn().Err(err).Str("namespace", ns).Msg("host approval: read failed")
			hostApprovalError(w, http.StatusInternalServerError, "error", "the request could not be read")
			return
		}
		if st.Status != persistence.ApprovalPending || !time.Now().Before(deadline) {
			writeHostApproval(w, st)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(min(hostApprovalPoll, time.Until(deadline))):
		}
	}
}

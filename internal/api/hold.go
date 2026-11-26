package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The held start (GitHub #586). A daemon started with the start-held marker
// serves its listeners behind HoldGate and starts no work until an
// admin-class DELETE /api/v1/daemon/hold releases it. The release reloads
// the config files, removes the marker, and only then starts work; the
// marker on disk is the one record of a release. A held daemon never
// releases itself: past its deadline it logs ERROR, reports overdue and
// alerts once per hold_id.
// see LLD § https://docs.vornik.io §13

// holdActionOverdue is the audit row that makes the overdue alert once per
// hold_id rather than once per process.
const holdActionOverdue = "daemon.hold.overdue"

// HoldInfo is what the marker said, for /readyz, GET and doctor.
type HoldInfo struct {
	HoldID string
	Reason string
	// Since is the deadline's base: the marker's created_at, or the process
	// start (BaseFromStart) when created_at was missing or in the future.
	Since         time.Time
	Deadline      time.Time
	BaseFromStart bool
	// MarkerErr says why the marker was malformed; it still holds.
	MarkerErr string
}

// HoldReleaser is the container's side of the release, called in this
// order under one mutex: ReloadConfig, RemoveMarker, StartWork; on a
// StartWork error RestoreMarker and then, after the 500 is flushed,
// WorkStartFailed (which ends the process).
type HoldReleaser interface {
	ReloadConfig() error
	RemoveMarker() error
	StartWork() error
	RestoreMarker() error
	WorkStartFailed(err error)
}

// HoldStatus is a snapshot of the hold for doctor and the GET endpoint.
type HoldStatus struct {
	Held          bool
	Info          HoldInfo
	Overdue       bool
	ReleaseFailed bool
	LastError     string
	// Redelivery is the GitHub redelivery pass after a release (#906): nil
	// before one, {"state":"running"} while it runs, then its report.
	Redelivery any
}

type holdState struct {
	held   atomic.Bool
	holdMu sync.Mutex
	// holdInfo is set once by WithStartHold, before serving, and only read
	// afterwards, so readers need no lock.
	holdInfo          HoldInfo
	holdReleaser      HoldReleaser
	holdAlert         func(ctx context.Context, subject, body string) error
	holdReleaseFailed bool
	holdLastError     string
	holdTimer         *time.Timer
	// holdAfterFunc is a seam (nil: time.AfterFunc) so a test fires the
	// deadline without sleeping, as drainAfterFunc does.
	holdAfterFunc func(time.Duration, func()) *time.Timer
	// holdAfterRelease is the redelivery pass (#906), nil when no GitHub
	// App can list its deliveries; holdRedelivery is its state, under holdMu.
	holdAfterRelease func(RedeliveryWindow) any
	holdRedelivery   any
}

// RedeliveryWindow is the time a released hold refused deliveries in
// (GitHub #906, §13.13): from the hold's Since to the release.
// FromProcessStart says Since was the process start, not the marker's
// created_at, so the restart's own gap is not covered.
type RedeliveryWindow struct {
	From, To         time.Time
	FromProcessStart bool
}

// WithAfterRelease sets the pass a successful release starts in the
// background (GitHub #906). It returns the report GET shows.
func WithAfterRelease(f func(RedeliveryWindow) any) ServerOption {
	return func(s *Server) { s.holdAfterRelease = f }
}

// WithStartHold starts the server held. alert sends the one overdue alert;
// nil only logs.
func WithStartHold(info HoldInfo, rel HoldReleaser, alert func(ctx context.Context, subject, body string) error) ServerOption {
	return func(s *Server) {
		s.holdInfo = info
		s.holdReleaser = rel
		s.holdAlert = alert
		s.held.Store(true)
	}
}

// IsHeld reports whether the daemon is held. The gate reads it in front of
// every handler; the release stores false only after work has started.
func (s *Server) IsHeld() bool { return s.held.Load() }

// HoldStatusNow is the hold as doctor and GET report it.
func (s *Server) HoldStatusNow() HoldStatus {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	return s.holdStatusLocked()
}

func (s *Server) holdStatusLocked() HoldStatus {
	st := HoldStatus{Held: s.held.Load(), ReleaseFailed: s.holdReleaseFailed, LastError: s.holdLastError, Redelivery: s.holdRedelivery}
	if st.Held {
		st.Info = s.holdInfo
		st.Overdue = s.holdOverdue()
	}
	return st
}

func (s *Server) holdOverdue() bool {
	d := s.holdInfo.Deadline
	return s.held.Load() && !d.IsZero() && !s.drainClock().Before(d)
}

// ArmHoldDeadline arms the overdue timer; Run calls it once the held daemon
// serves. A deadline already past fires at once (a restart after it).
func (s *Server) ArmHoldDeadline() {
	if !s.held.Load() {
		return
	}
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	d := s.holdInfo.Deadline.Sub(s.drainClock())
	if d < 0 {
		d = 0
	}
	after := s.holdAfterFunc
	if after == nil {
		after = time.AfterFunc
	}
	s.holdTimer = after(d, s.holdDeadlinePassed)
}

// holdDeadlinePassed logs, records and alerts once per hold_id. It never
// releases: past the deadline the verifier died or failed, and releasing
// would start work a rollback then erases (§13.6).
func (s *Server) holdDeadlinePassed() {
	s.holdMu.Lock()
	held := s.held.Load()
	info := s.holdInfo
	s.holdMu.Unlock()
	if !held {
		return
	}
	s.logger.Error().Str("hold_id", info.HoldID).Str("reason", info.Reason).
		Time("since", info.Since).Time("deadline", info.Deadline).
		Msg("held start overdue: no release before the deadline; the daemon stays held. Release with `vornikctl daemon release`, or roll back")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !s.claimOverdueAlert(ctx, info) {
		return
	}
	if s.holdAlert == nil {
		return
	}
	body := "The daemon started held (hold " + info.HoldID + ", " + info.Reason + ") at " +
		info.Since.UTC().Format(time.RFC3339) + " and was not released by " +
		info.Deadline.UTC().Format(time.RFC3339) + ". It does no work until released (vornikctl daemon release) or rolled back."
	if err := s.holdAlert(ctx, "Vornik held start overdue", body); err != nil {
		s.logger.Error().Err(err).Str("hold_id", info.HoldID).Msg("held start overdue: the operator alert could not be sent")
	}
}

// claimOverdueAlert reports whether this process should alert: no audit row
// for this hold_id yet (it writes one), or no way to check. A repeated alert
// is accepted; a lost one is not.
func (s *Server) claimOverdueAlert(ctx context.Context, info HoldInfo) bool {
	if s.adminAuditRepo == nil || info.HoldID == "" {
		s.logger.Warn().Msg("held start overdue: cannot check whether this hold already alerted (no audit store or no hold_id); alerting")
		return true
	}
	rows, err := s.adminAuditRepo.List(ctx, persistence.AdminAuditFilter{
		Action: holdActionOverdue, TargetPrefix: info.HoldID, PageSize: 50})
	if err != nil {
		s.logger.Warn().Err(err).Msg("held start overdue: the audit lookup failed; alerting")
		return true
	}
	for _, r := range rows {
		if r.Target == info.HoldID {
			return false
		}
	}
	after, _ := json.Marshal(map[string]any{
		"reason": info.Reason, "since": info.Since.UTC().Format(time.RFC3339),
		"deadline": info.Deadline.UTC().Format(time.RFC3339)})
	if err := s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: "daemon", Source: "daemon", Action: holdActionOverdue,
		Target: info.HoldID, After: string(after)}); err != nil {
		s.logger.Warn().Err(err).Msg("held start overdue: the audit row could not be written; a restart may alert again")
	}
	return true
}

type daemonHoldResponse struct {
	Held          bool   `json:"held"`
	HoldID        string `json:"hold_id,omitempty"`
	Since         string `json:"since,omitempty"`
	Deadline      string `json:"deadline,omitempty"`
	Overdue       bool   `json:"overdue"`
	Reason        string `json:"reason,omitempty"`
	MarkerError   string `json:"marker_error,omitempty"`
	ReleaseFailed bool   `json:"release_failed,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// Redelivery is "started" or "not_configured" on the release's answer,
	// then the pass's state on GET (#906, §13.13).
	Redelivery any `json:"redelivery,omitempty"`
}

func holdResponse(st HoldStatus) daemonHoldResponse {
	out := daemonHoldResponse{Held: st.Held, Overdue: st.Overdue, ReleaseFailed: st.ReleaseFailed, LastError: st.LastError, Redelivery: st.Redelivery}
	if st.Held {
		out.HoldID, out.Reason, out.MarkerError = st.Info.HoldID, st.Info.Reason, st.Info.MarkerErr
		if !st.Info.Since.IsZero() {
			out.Since = st.Info.Since.UTC().Format(time.RFC3339)
		}
		if !st.Info.Deadline.IsZero() {
			out.Deadline = st.Info.Deadline.UTC().Format(time.RFC3339)
		}
	}
	return out
}

// DaemonHold serves /api/v1/daemon/hold: GET reports the hold, DELETE
// releases it. Admin-class only, through the one admin-class predicate, as
// the drain endpoint (§13.4).
func (s *Server) DaemonHold(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET for the hold's status or DELETE to release it")
		return
	}
	if !s.isAdminClassRequest(r) {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED",
			"the held start needs an admin-class key (admin.allowed_keys, with admin.enabled) or an admin session")
		return
	}
	if r.Method == http.MethodGet {
		respondJSON(w, http.StatusOK, holdResponse(s.HoldStatusNow()))
		return
	}
	s.releaseHold(w, r)
}

// shuttingDown reads the shutdown drain. releaseHold calls it holding
// holdMu, so the lock order is holdMu, then drainMu; nothing takes them the
// other way round.
func (s *Server) shuttingDown() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.drainShutdown
}

// releaseHold is the DELETE: reload, remove the marker, start work, open
// the gate, all under holdMu so work starts at most once (§13.4, §13.11.3).
func (s *Server) releaseHold(w http.ResponseWriter, r *http.Request) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if s.shuttingDown() {
		respondError(w, http.StatusConflict, "SHUTTING_DOWN", "the daemon is shutting down; nothing was released")
		return
	}
	if s.holdReleaseFailed {
		respondError(w, http.StatusConflict, "RELEASE_FAILED",
			"a release already failed to start work and the process is exiting; the next process decides (do not retry)")
		return
	}
	if !s.held.Load() {
		respondJSON(w, http.StatusOK, holdResponse(s.holdStatusLocked()))
		return
	}
	rel := s.holdReleaser
	principal := drainPrincipal(r)
	if err := rel.ReloadConfig(); err != nil {
		s.holdLastError = "config reload: " + err.Error()
		s.logger.Error().Err(err).Str("principal", principal).
			Msg("held start: release refused, the config reload failed; still held, nothing started")
		respondError(w, http.StatusInternalServerError, "CONFIG_RELOAD_FAILED",
			"the config reload failed, so nothing was released or started: "+err.Error()+
				". Fix the files and release again; a conflict with in-flight tasks clears by reverting the edit or by restarting the daemon (it comes back held)")
		return
	}
	if err := rel.RemoveMarker(); err != nil {
		s.holdLastError = "marker not removed: " + err.Error()
		s.logger.Error().Err(err).Msg("held start: release refused, the start-held marker could not be removed; still held, nothing started")
		respondError(w, http.StatusInternalServerError, "MARKER_NOT_REMOVED",
			"the start-held marker could not be removed, so nothing was started: "+err.Error())
		return
	}
	if err := rel.StartWork(); err != nil {
		s.holdReleaseFailed = true
		s.holdLastError = "work start: " + err.Error()
		if rerr := rel.RestoreMarker(); rerr != nil {
			s.logger.Error().Err(rerr).Msg("held start: the marker could not be written back; the next start is not held and fails its work start before serving")
		}
		s.logger.Error().Err(err).Msg("held start: the release failed to start work; the process exits")
		respondError(w, http.StatusInternalServerError, "WORK_START_FAILED",
			"the release could not start work and the daemon is exiting: "+err.Error())
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		rel.WorkStartFailed(err)
		return
	}
	info := s.holdInfo
	s.held.Store(false)
	s.holdLastError = ""
	if s.holdTimer != nil {
		s.holdTimer.Stop()
		s.holdTimer = nil
	}
	held := s.drainClock().Sub(info.Since).Round(time.Second)
	s.logger.Info().Str("principal", principal).Str("hold_id", info.HoldID).Dur("held_for", held).
		Msg("held start released: work started")
	s.auditDrain(r, "daemon.release", map[string]any{
		"hold_id": info.HoldID, "reason": info.Reason, "held_seconds": int64(held / time.Second)})
	out := holdResponse(s.holdStatusLocked())
	out.Redelivery = s.startRedelivery(r, info)
	respondJSON(w, http.StatusOK, out)
}

// startRedelivery starts the GitHub redelivery pass after a release, in the
// background, so it can never slow or fail the release (#906, §13.13). The
// gate is already open: a redelivery to a held daemon would get 503 again.
// Called holding holdMu.
func (s *Server) startRedelivery(r *http.Request, info HoldInfo) string {
	pass := s.holdAfterRelease
	if pass == nil {
		return "not_configured"
	}
	win := RedeliveryWindow{From: info.Since, To: s.drainClock(), FromProcessStart: info.BaseFromStart}
	// The audit row is written after the handler returned: keep a detached
	// copy of the request, never the live one (review d94e #1).
	ar := r.Clone(context.WithoutCancel(r.Context()))
	s.holdRedelivery = map[string]string{"state": "running"}
	go func() {
		rep := pass(win)
		if rep == nil {
			rep = map[string]string{"state": "done"}
		}
		s.holdMu.Lock()
		s.holdRedelivery = rep
		s.holdMu.Unlock()
		s.logger.Info().Str("hold_id", info.HoldID).Interface("redelivery", rep).
			Msg("held start released: GitHub redelivery pass finished")
		s.auditDrain(ar, "daemon.release.redelivery", map[string]any{"hold_id": info.HoldID, "report": rep})
	}()
	return "started"
}

// HoldAllowed is the held gate's allowlist (§13.2, §13.11.2): the probes,
// the read-only doctor, and the hold endpoint. An allowlist, not a
// denylist: a route added later is refused while held until it is added
// here.
func HoldAllowed(r *http.Request) bool {
	p := r.URL.Path
	// Only a clean path can match: the gate and the mux must agree on what
	// a path names, and a dot segment or a doubled slash could otherwise
	// pass the prefix match and name a sibling after cleaning (review 252e
	// #1). ServeMux redirects such a path; refusing it here is the same
	// answer without relying on that.
	if p == "" || path.Clean(p) != p {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		switch p {
		case "/livez", "/healthz", "/readyz", "/health/live", "/health/ready", "/metrics",
			"/api/v1/doctor/features", "/api/v1/daemon/hold":
			return true
		}
		return strings.HasPrefix(p, "/api/v1/doctor/features/")
	case http.MethodPost:
		// The doctor run; with fix=true it repairs rows, which is a write.
		// RunDoctor reads the flag the same way (Query().Get("fix") ==
		// "true"), so the gate refuses exactly the runs that write.
		return p == "/api/v1/doctor" && r.URL.Query().Get("fix") != "true"
	case http.MethodDelete:
		return p == "/api/v1/daemon/hold"
	}
	return false
}

// HoldGate refuses, while held() is true, every request that allowed does
// not pass (nil allows nothing: the relay ingress and the agent socket serve
// nothing while held), with 503, Retry-After 30 and {"status":"held"}.
// It goes outermost on each listener so nothing routes around it.
func HoldGate(held func() bool, allowed func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if held() && (allowed == nil || !allowed(r)) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"held"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

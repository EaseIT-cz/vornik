package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Operator drain (GitHub #290). The deploy idle gate used to read the
// chat-proxy in-flight count as 0 and only then did SIGTERM set the drain
// bit, so a request that started in between was admitted and could be cut at
// shutdown Phase 3. The operator drain sets the same bit FIRST, as a lease
// that clears itself after a TTL unless renewed; the gate then reads the
// count after the bit. SIGTERM (SetDraining(true)) makes the drain permanent.
// see LLD § https://docs.vornik.io §9

const (
	defaultOperatorDrainTTL = 300 * time.Second
	maxOperatorDrainTTL     = 3600 * time.Second
	drainBodyLimit          = 4 << 10
)

// errDrainShuttingDown is returned by EndOperatorDrain once SIGTERM has made
// the drain permanent.
var errDrainShuttingDown = errors.New("the daemon is shutting down; the drain cannot be cleared")

// drainState is the writer side of the drain bit (s.draining). All writers
// hold drainMu; readers on the hot path read the atomic bit only.
type drainState struct {
	drainMu       sync.Mutex
	drainSince    time.Time   // when the bit was set; zero when clear
	drainExpires  time.Time   // operator-drain deadline; zero when none
	drainTimer    *time.Timer // the operator-drain expiry; nil when none
	drainGen      uint64      // bumped by every write; a stale expiry is ignored
	drainShutdown bool        // SIGTERM drain: permanent

	// drainAfterFunc and drainNow are seams (nil: time.AfterFunc, time.Now)
	// so a test fires expiry and moves the clock without sleeping.
	// drainAfterFunc is called with drainMu held and must not run fn before
	// it returns (fn takes drainMu); a fake captures fn and fires it later
	// (review f072 #4).
	drainAfterFunc func(time.Duration, func()) *time.Timer
	drainNow       func() time.Time
}

// DrainState is a snapshot of the drain for the endpoint and the shutdown
// path.
type DrainState struct {
	Draining     bool
	ShuttingDown bool
	Since        time.Time
	ExpiresAt    time.Time
}

// WithDrainClock replaces the drain's clock (tests of the shutdown grace).
func WithDrainClock(now func() time.Time) ServerOption {
	return func(s *Server) { s.drainNow = now }
}

func (s *Server) drainClock() time.Time {
	if s.drainNow != nil {
		return s.drainNow()
	}
	return time.Now()
}

func (s *Server) stopDrainTimerLocked() {
	if s.drainTimer != nil {
		s.drainTimer.Stop()
		s.drainTimer = nil
	}
	s.drainExpires = time.Time{}
}

func (s *Server) drainSnapshotLocked() DrainState {
	return DrainState{
		Draining:     s.draining.Load(),
		ShuttingDown: s.drainShutdown,
		Since:        s.drainSince,
		ExpiresAt:    s.drainExpires,
	}
}

// BeginOperatorDrain sets the drain bit as a lease of ttl. Idempotent: a call
// while drained renews the deadline (the latest call wins) and keeps Since.
// Once SIGTERM has made the drain permanent it changes nothing.
func (s *Server) BeginOperatorDrain(ttl time.Duration) (DrainState, error) {
	if ttl <= 0 || ttl > maxOperatorDrainTTL {
		return DrainState{}, errors.New("ttl out of range")
	}
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if s.drainShutdown {
		return s.drainSnapshotLocked(), nil
	}
	now := s.drainClock()
	s.stopDrainTimerLocked()
	s.drainGen++
	gen := s.drainGen
	if !s.draining.Load() || s.drainSince.IsZero() {
		s.drainSince = now
	}
	s.draining.Store(true)
	s.drainExpires = now.Add(ttl)
	after := s.drainAfterFunc
	if after == nil {
		after = time.AfterFunc
	}
	s.drainTimer = after(ttl, func() { s.expireOperatorDrain(gen) })
	return s.drainSnapshotLocked(), nil
}

// EndOperatorDrain clears an operator drain. Idempotent on a daemon that is
// not drained; refused once SIGTERM has made the drain permanent.
func (s *Server) EndOperatorDrain() error {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if s.drainShutdown {
		return errDrainShuttingDown
	}
	s.drainGen++
	s.stopDrainTimerLocked()
	s.drainSince = time.Time{}
	s.draining.Store(false)
	return nil
}

// expireOperatorDrain clears the bit when the lease armed at generation gen
// is still the current one. A renewal, an undrain or SIGTERM after it was
// armed bumped the generation (or set drainShutdown), so it does nothing.
func (s *Server) expireOperatorDrain(gen uint64) {
	s.drainMu.Lock()
	if gen != s.drainGen || s.drainShutdown || !s.draining.Load() {
		s.drainMu.Unlock()
		return
	}
	since := s.drainSince
	s.drainGen++
	s.drainTimer = nil
	s.drainExpires = time.Time{}
	s.drainSince = time.Time{}
	s.draining.Store(false)
	s.drainMu.Unlock()

	s.logger.Warn().Time("drained_since", since).
		Msg("operator drain expired; accepting chat-proxy requests again")
	if s.adminAuditRepo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		after, _ := json.Marshal(map[string]any{"drained_since": since.UTC().Format(time.RFC3339)})
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: "daemon",
			Source:    "daemon",
			Action:    "daemon.drain.expired",
			After:     string(after),
		})
	}
}

// DrainedFor is how long the drain bit has been set; 0 when it is clear.
func (s *Server) DrainedFor() time.Duration {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.drainedForLocked()
}

func (s *Server) drainedForLocked() time.Duration {
	if !s.draining.Load() || s.drainSince.IsZero() {
		return 0
	}
	if d := s.drainClock().Sub(s.drainSince); d > 0 {
		return d
	}
	return 0
}

// BeginShutdownDrain is the SIGTERM drain: it sets the bit permanently (no
// expiry or undrain clears it afterwards) and returns how long the bit had
// already been set by an operator drain, 0 when it was clear. One critical
// section, so a concurrent undrain or a firing expiry cannot fall between
// the read and the set (design review e381 #1). shuttingDown and the
// generation change before the timer is stopped, all under the mutex: an
// expiry callback already running blocks on the mutex and then finds a stale
// generation (review e381 #4).
func (s *Server) BeginShutdownDrain() time.Duration {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	already := s.drainedForLocked()
	s.drainShutdown = true
	s.drainGen++
	s.stopDrainTimerLocked()
	if already == 0 || s.drainSince.IsZero() {
		s.drainSince = s.drainClock()
	}
	s.draining.Store(true)
	return already
}

// daemonDrainResponse is the body of POST and DELETE /api/v1/daemon/drain.
type daemonDrainResponse struct {
	Draining     bool   `json:"draining"`
	ShuttingDown bool   `json:"shutting_down"`
	Since        string `json:"since,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	ChatProxy    int64  `json:"chat_proxy"`
	Total        int64  `json:"total"`
}

func (s *Server) drainResponse(st DrainState) daemonDrainResponse {
	n := s.InflightChatProxy()
	out := daemonDrainResponse{Draining: st.Draining, ShuttingDown: st.ShuttingDown, ChatProxy: n, Total: n}
	if st.Draining && !st.Since.IsZero() {
		out.Since = st.Since.UTC().Format(time.RFC3339)
	}
	if !st.ShuttingDown && !st.ExpiresAt.IsZero() {
		out.ExpiresAt = st.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return out
}

// parseDrainTTL reads the optional {"ttl_seconds": N}; an empty body or an
// absent field is the default.
func parseDrainTTL(r *http.Request) (time.Duration, bool) {
	if r.Body == nil {
		return defaultOperatorDrainTTL, true
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, drainBodyLimit+1))
	if err != nil || len(raw) > drainBodyLimit {
		return 0, false
	}
	if strings.TrimSpace(string(raw)) == "" {
		return defaultOperatorDrainTTL, true
	}
	var body struct {
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, false
	}
	if body.TTLSeconds == nil {
		return defaultOperatorDrainTTL, true
	}
	n := *body.TTLSeconds
	if n < 1 || n > int64(maxOperatorDrainTTL/time.Second) {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// DaemonDrain serves /api/v1/daemon/drain: POST sets (or renews) the
// operator drain, DELETE clears it. Admin-class only, in both editions,
// through the one admin-class predicate (isAdminClassRequest).
func (s *Server) DaemonDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST to drain or DELETE to undrain")
		return
	}
	if !s.isAdminClassRequest(r) {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED",
			"draining the daemon needs an admin-class key (admin.allowed_keys, with admin.enabled) or an admin session")
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.EndOperatorDrain(); err != nil {
			respondError(w, http.StatusConflict, "SHUTTING_DOWN", err.Error())
			return
		}
		s.logger.Info().Str("principal", drainPrincipal(r)).Msg("operator drain cleared")
		s.auditDrain(r, "daemon.undrain", map[string]any{"draining": false})
		respondJSON(w, http.StatusOK, s.drainResponse(DrainState{}))
		return
	}
	ttl, ok := parseDrainTTL(r)
	if !ok {
		respondError(w, http.StatusBadRequest, "INVALID_TTL", "ttl_seconds must be a whole number from 1 to 3600")
		return
	}
	st, err := s.BeginOperatorDrain(ttl)
	if err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_TTL", err.Error())
		return
	}
	resp := s.drainResponse(st)
	if !st.ShuttingDown {
		s.logger.Info().Str("principal", drainPrincipal(r)).Dur("ttl", ttl).
			Time("expires_at", st.ExpiresAt).Int64("in_flight", resp.Total).
			Msg("operator drain set: refusing new chat-proxy requests")
		s.auditDrain(r, "daemon.drain", map[string]any{
			"ttl_seconds": int64(ttl / time.Second),
			"expires_at":  resp.ExpiresAt,
			"since":       resp.Since,
		})
	}
	respondJSON(w, http.StatusOK, resp)
}

// drainPrincipal names who drained, never with a bearer secret: a session
// admin by user ID (as assistActor does), a key by its principal (review
// f072 #3).
func drainPrincipal(r *http.Request) string {
	if p := RequestPrincipal(r.Context()); p != "" {
		return p
	}
	return "anonymous-admin"
}

func (s *Server) auditDrain(r *http.Request, action string, after map[string]any) {
	if s.adminAuditRepo == nil {
		return
	}
	body, _ := json.Marshal(after)
	// Detached from the request: a client that hangs up after the drain took
	// effect must not drop its audit row (design review 27d8).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: drainPrincipal(r),
		Source:    "api",
		Action:    action,
		After:     string(body),
		IP:        clientIPFromRequest(r),
		UserAgent: r.UserAgent(),
	})
}

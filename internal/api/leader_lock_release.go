package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// POST /api/v1/admin/leader-locks/release — backs `vornikctl leader-lock
// release` (issue #60; horizontal scaling LLD, amendment 2026-09-23 and its
// implementation contract 2026-09-25).
//
// THE PREDICATE IS THE CONTROL. Each requested row gets exactly one
// DeleteExpired, whose `expires_at < now` decides; nothing here classifies a
// row and then mutates it. A refusal is labelled by an ADVISORY read after the
// statement returned nothing, and no mutation follows that read. There is no
// force: against a live holder a DELETE neither evicts it (it re-inserts
// within a heartbeat) nor keeps the epoch fence (it resets to 1).

// LeaderLockReleaseMetrics counts release outcomes by {outcome, mode}. NOT
// keyed by worker: worker ids embed project ids and this table has no tenant
// boundary yet.
type LeaderLockReleaseMetrics struct {
	total *prometheus.CounterVec
}

// NewLeaderLockReleaseMetrics registers the counter. Build it ONCE, on the
// served registry (the container's pass-2 rule), and hand it to
// WithLeaderLockRelease: registering inside the option would panic on the
// second server build.
func NewLeaderLockReleaseMetrics(registerer prometheus.Registerer) *LeaderLockReleaseMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &LeaderLockReleaseMetrics{total: promauto.With(registerer).NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "leader_lock_release_total",
			Help: "Rows asked of `vornikctl leader-lock release`, by {outcome, mode}. outcome = " +
				"released|refused-stale|refused-active|refused-changed|unknown; mode = named|bulk. " +
				"Deliberately not keyed by worker: worker ids embed project ids.",
		},
		[]string{"outcome", "mode"},
	)}
}

func (m *LeaderLockReleaseMetrics) record(outcome, mode string) {
	if m == nil {
		return
	}
	m.total.WithLabelValues(outcome, mode).Inc()
}

// WithLeaderLockRelease wires the release endpoint: the leader-lock
// repository and the process's wired worker ids (the doctor's own source, used
// by --all-orphaned). Unwired, the endpoint answers 503.
func WithLeaderLockRelease(repo persistence.DaemonLeaderLockRepository, wired func() []string) ServerOption {
	return func(s *Server) {
		s.leaderLockRepo = repo
		s.leaderLockWired = wired
		s.leaderLockNow = func() time.Time { return time.Now().UTC() }
	}
}

// WithLeaderLockReleaseMetrics wires the outcome counter, separately from
// WithLeaderLockRelease because the container builds metrics later in the same
// pass, on the served registry. Nil-safe.
func WithLeaderLockReleaseMetrics(m *LeaderLockReleaseMetrics) ServerOption {
	return func(s *Server) {
		s.leaderLockReleaseMetrics = m
	}
}

type leaderLockReleaseRequest struct {
	WorkerIDs   []string `json:"worker_ids"`
	AllOrphaned bool     `json:"all_orphaned"`
	Reason      string   `json:"reason"`
}

type leaderLockReleaseResult struct {
	WorkerID  string     `json:"worker_id"`
	Outcome   string     `json:"outcome"`
	Message   string     `json:"message"`
	HolderID  string     `json:"holder_id,omitempty"`
	Epoch     int64      `json:"epoch,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Advisory marks a refusal's label: it comes from a read AFTER the
	// statement refused, and the row may have changed again since.
	Advisory bool `json:"advisory,omitempty"`
	// AuditError is set when a released row's admin_audit write failed. The
	// row IS released; the failure is reported, not hidden.
	AuditError string `json:"audit_error,omitempty"`
}

type leaderLockReleaseResponse struct {
	Results []leaderLockReleaseResult `json:"results"`
	// KnownWorkerIDs is listed on an `unknown` outcome for an authenticated
	// admin only: worker ids embed project ids, so it is elided with auth off.
	KnownWorkerIDs []string `json:"known_worker_ids,omitempty"`
}

// LeaderLockRelease handles POST /api/v1/admin/leader-locks/release.
func (s *Server) LeaderLockRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.leaderLockRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "LEADER_LOCKS_UNWIRED", "leader-lock release is not wired into this daemon")
		return
	}
	var req leaderLockReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "body must be JSON: {worker_ids | all_orphaned, reason}")
		return
	}
	if (len(req.WorkerIDs) > 0) == req.AllOrphaned {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "give exactly one of worker_ids and all_orphaned")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.AllOrphaned && req.Reason == "" {
		respondError(w, http.StatusBadRequest, "REASON_REQUIRED", "--all-orphaned requires --reason")
		return
	}

	// ONE now per request: every statement, every advisory classification and
	// the --all-orphaned target derivation use it, so per-row outcomes are
	// comparable and the exit code cannot depend on how long the loop took.
	now := s.leaderLockNow()
	ctx := r.Context()
	mode, targets := "named", req.WorkerIDs
	if req.AllOrphaned {
		mode = "bulk"
		var err error
		if targets, err = s.orphanedExpiredLocks(ctx, now); err != nil {
			respondError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
			return
		}
	}

	resp := leaderLockReleaseResponse{Results: make([]leaderLockReleaseResult, 0, len(targets))}
	for _, id := range targets {
		res, err := s.releaseOneLeaderLock(ctx, r, id, now, req.Reason, req.AllOrphaned)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "RELEASE_FAILED", fmt.Sprintf("%s: %v", id, err))
			return
		}
		s.leaderLockReleaseMetrics.record(res.Outcome, mode)
		resp.Results = append(resp.Results, res)
	}
	if IsAuthEnabledFromContext(ctx) && hasUnknown(resp.Results) {
		resp.KnownWorkerIDs = s.knownLeaderLockIDs(ctx)
	}
	respondJSON(w, http.StatusOK, resp)
}

// orphanedExpiredLocks derives the --all-orphaned target set at execution time
// from the same inputs the doctor reads: rows not wired in this process and
// expired as of now. Each is then released by its own DeleteExpired, so a row
// renewed in between is refused by the statement, not taken.
func (s *Server) orphanedExpiredLocks(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.leaderLockRepo.List(ctx)
	if err != nil {
		return nil, err
	}
	wired := map[string]bool{}
	if s.leaderLockWired != nil {
		for _, id := range s.leaderLockWired() {
			wired[id] = true
		}
	}
	var out []string
	for _, row := range rows {
		if !wired[row.WorkerID] && persistence.LeaderLockExpired(row.ExpiresAt, now) {
			out = append(out, row.WorkerID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Server) releaseOneLeaderLock(ctx context.Context, r *http.Request, id string, now time.Time, reason string, bulk bool) (leaderLockReleaseResult, error) {
	res := leaderLockReleaseResult{WorkerID: id}
	row, err := s.leaderLockRepo.DeleteExpired(ctx, id, now)
	if err != nil {
		return res, err
	}
	if row != nil {
		expires := row.ExpiresAt
		res.Outcome, res.HolderID, res.Epoch, res.ExpiresAt = leaderelection.OutcomeReleased, row.HolderID, row.Epoch, &expires
		res.Message = fmt.Sprintf("released: held by %s at epoch %d, expired %s ago",
			row.HolderID, row.Epoch, humanLeaderLockDuration(now.Sub(row.ExpiresAt)))
		if aerr := s.auditLeaderLockRelease(ctx, r, row, reason, bulk); aerr != nil {
			res.AuditError = aerr.Error()
		}
		return res, nil
	}
	// Refused. Label it from an advisory read, against the SAME now.
	res.Advisory = true
	cur, gerr := s.leaderLockRepo.Get(ctx, id)
	if gerr != nil && !errors.Is(gerr, persistence.ErrNotFound) {
		return res, gerr
	}
	if cur == nil {
		res.Outcome = leaderelection.OutcomeUnknown
		res.Message = "no leader lock for this worker id: it never existed, or another release removed it first"
		return res, nil
	}
	switch state, detail := classifyLeaderLock(cur, now); state {
	case "STALE":
		res.Outcome = leaderelection.OutcomeRefusedStale
		res.Message = fmt.Sprintf("refused, not expired (%s); a stale row ages into an expired one, so re-run after the lease runs out", detail)
	case "EXPIRED":
		res.Outcome = leaderelection.OutcomeRefusedChanged
		res.Message = "refused: the row was replaced between the release and the read; re-run"
	default:
		res.Outcome = leaderelection.OutcomeRefusedActive
		res.Message = fmt.Sprintf("refused, the lease is live (%s): restart the holder (SIGTERM drains and releases it), SIGKILL if it will not drain, or wait out the TTL", detail)
	}
	return res, nil
}

// auditLeaderLockRelease writes one admin_audit row from the row the
// statement RETURNED, never from a re-read: holder and epoch are the facts a
// reason string cannot carry.
func (s *Server) auditLeaderLockRelease(ctx context.Context, r *http.Request, row *persistence.DaemonLeaderLock, reason string, bulk bool) error {
	if s.adminAuditRepo == nil {
		return errors.New("no admin audit log is wired")
	}
	after, _ := json.Marshal(map[string]any{
		"holder_id": row.HolderID, "epoch": row.Epoch, "expires_at": row.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"reason": reason, "bulk": bulk,
	})
	principal := apiKeyPrincipalFromContext(r.Context())
	if principal == "" {
		principal = "anonymous-admin"
	}
	if err := s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: principal, Source: "api", Action: "leader_lock.release", Target: row.WorkerID,
		After: string(after), IP: clientIPFromRequest(r), UserAgent: r.UserAgent(),
	}); err != nil {
		s.logger.Warn().Err(err).Str("worker_id", row.WorkerID).Msg("leader-lock release: audit write failed; the row IS released")
		return err
	}
	return nil
}

func hasUnknown(results []leaderLockReleaseResult) bool {
	for _, r := range results {
		if r.Outcome == leaderelection.OutcomeUnknown {
			return true
		}
	}
	return false
}

func (s *Server) knownLeaderLockIDs(ctx context.Context) []string {
	rows, err := s.leaderLockRepo.List(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.WorkerID)
	}
	sort.Strings(out)
	return out
}

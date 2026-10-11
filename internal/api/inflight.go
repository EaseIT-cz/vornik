package api

import (
	"net/http"
	"sync/atomic"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// longRequestCounter counts in-flight long requests by kind. One kind today:
// the chat-proxy routes (OpenAI-shaped and Ollama compat), which may run for
// minutes and which the deploy idle gate could not see (GitHub #97).
// see LLD § https://docs.vornik.io §8
type longRequestCounter struct {
	chatProxy atomic.Int64
}

// drainRetryAfterSeconds is the Retry-After a drain refusal carries: a
// restart is back well within it.
const drainRetryAfterSeconds = "30"

// trackLongRequest wraps a chat-proxy handler. Once the drain bit is set
// (beginDrain, the same bit /readyz reads) a new request is refused with 503
// DRAINING and Retry-After; otherwise it is counted for its whole duration.
// The decrement is deferred, so an error return, a client disconnect or a
// panic recovered further out all leave the count right.
func (s *Server) trackLongRequest(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			taskID, ok := s.runningAgentTaskKey(r)
			if !ok {
				w.Header().Set("Retry-After", drainRetryAfterSeconds)
				respondError(w, http.StatusServiceUnavailable, "DRAINING",
					"the daemon is restarting; retry shortly")
				return
			}
			// A running agent step is paused by shutdown Phase 2, not cut
			// here; still counted below so the gate waits for it.
			s.logger.Info().Str("task_id", taskID).
				Msg("served during drain: running task key")
		}
		s.inflight.chatProxy.Add(1)
		defer s.inflight.chatProxy.Add(-1)
		h(w, r)
	}
}

// runningAgentTaskKey reports whether r authenticated with the per-task key
// the executor minted for an agent container (a DB key row named with the
// reserved task prefix, empty client_kind, created_by "executor") whose task
// is RUNNING or LEASED, and returns that task's ID. No request header takes
// part. Any failure answers false, so the caller refuses (GitHub #97,
// implementation review 5860 #2).
// see LLD § https://docs.vornik.io §8.5a
func (s *Server) runningAgentTaskKey(r *http.Request) (string, bool) {
	id := IdentityFromContext(r.Context())
	if id == nil {
		return "", false
	}
	row, ok := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey)
	if !ok || row == nil || row.ClientKind != "" || row.CreatedBy != executorKeyCreator {
		return "", false
	}
	taskID, isTaskKey := persistence.TaskIDFromKeyName(row.Name)
	if !isTaskKey || s.taskRepo == nil {
		return "", false
	}
	task, err := s.taskRepo.Get(r.Context(), taskID)
	if err != nil || task == nil {
		return "", false
	}
	if task.Status != persistence.TaskStatusRunning && task.Status != persistence.TaskStatusLeased {
		return "", false
	}
	return taskID, true
}

// executorKeyCreator is the created_by the executor's per-task key minter
// (taskkeys.MintTaskKey) writes; no grant or rotation path writes it.
const executorKeyCreator = "executor"

// InflightChatProxy is the number of chat-proxy requests in flight.
func (s *Server) InflightChatProxy() int64 { return s.inflight.chatProxy.Load() }

// daemonInflightResponse is GET /api/v1/daemon/inflight. Total is the sum
// over every counted kind, so a reader keeps working when a kind is added.
type daemonInflightResponse struct {
	ChatProxy int64 `json:"chat_proxy"`
	Total     int64 `json:"total"`
	Draining  bool  `json:"draining"`
}

// DaemonInflight serves GET /api/v1/daemon/inflight for the deploy idle gate
// (`vornikctl daemon inflight`). Admin-class only: auth off passes, otherwise
// an admin-class key or an admin session, in both editions.
func (s *Server) DaemonInflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if !s.requireAdminClassGate(w, r) {
		return
	}
	n := s.InflightChatProxy()
	respondJSON(w, http.StatusOK, daemonInflightResponse{ChatProxy: n, Total: n, Draining: s.draining.Load()})
}

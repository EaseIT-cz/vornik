package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

// GetTaskLogs handles GET /api/v1/projects/{projectId}/tasks/{taskId}/logs.
func (s *Server) GetTaskLogs(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	taskID := extractTaskID(r)
	if projectID == "" || taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and taskId are required")
		return
	}

	// GitHub #491: an unscoped caller that is not admin-class reads no log,
	// decided before the scrubber is consulted.
	if s.errorTextAccess(r) == ErrorTextDenied {
		RespondProjectScopeRequired(w)
		return
	}

	tail := parseTailParam(r, 200)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	held, err := s.taskLogView(r)
	if err != nil {
		respondLogScrubberUnavailable(w)
		return
	}
	logs, err := s.taskLogs(ctx, held, projectID, taskID, tail)
	if err != nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
}

func parseTailParam(r *http.Request, fallback int) int {
	tail := fallback
	if raw := r.URL.Query().Get("tail"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			tail = n
		}
	}
	if tail > 5000 {
		tail = 5000
	}
	return tail
}

func (s *Server) taskLogs(ctx context.Context, held HeldTaskLog, projectID, taskID string, tail int) (string, error) {
	if s.taskRepo != nil {
		task, err := s.taskRepo.Get(ctx, taskID)
		if err != nil || task == nil {
			return "", fmt.Errorf("task not found")
		}
		if task.ProjectID != projectID {
			return "", fmt.Errorf("task does not belong to project")
		}
	}

	// GitHub #294: daemon-held state only. This path used to ask the
	// executor, which ran `podman logs` on the request (process-spawn law).
	logs, err := held.TaskLogs(ctx, taskID, tail)
	if err != nil {
		return "", err
	}
	return trimLines(logs, tail), nil
}

// taskLogView is the held task log a request reads (GitHub #440;
// process-spawn-law design, "Amendment 2026-10-21 (#439, #440)", C3): the
// operator view for an admin-class caller (isAdminClassRequest: auth off, an
// admin session, an admin.allowed_keys key), the scrubbed view for every
// other caller. It errors when the scrubbed view is needed and the scrubber
// could not be built; the route then serves nothing. GitHub #491: it also
// errors for an ErrorTextDenied caller, so a caller that skipped the route's
// own check still reads nothing.
func (s *Server) taskLogView(r *http.Request) (HeldTaskLog, error) {
	held := s.heldTaskLog()
	switch s.errorTextAccess(r) {
	case ErrorTextOperator:
		return held, nil
	case ErrorTextScrubbed:
		return held.ScrubbedView(s.liveLogs().scrub)
	default:
		return HeldTaskLog{}, errUnscopedCaller
	}
}

// errUnscopedCaller is taskLogView's refusal of an ErrorTextDenied caller.
var errUnscopedCaller = errors.New("the caller is not scoped to a project")

// errorTextView is how a request reads a persisted error text (a task's
// last_error, an execution's error_message; GitHub #466, process-spawn-law
// design, "Amendment 2026-10-21 (#466)", D1): unchanged for an admin-class
// caller, report.ScrubLines with the server's scrubber for every other. With
// no scrubber ScrubLines withholds every line, so the read is still served
// and nothing raw is. GitHub #491 (design K2): an ErrorTextDenied caller
// reads the constant UnscopedWithheldText in place of the whole field.
func (s *Server) errorTextView(r *http.Request) func(string) string {
	switch s.errorTextAccess(r) {
	case ErrorTextOperator:
		return func(t string) string { return t }
	case ErrorTextScrubbed:
		return s.scrubbedErrorText
	default:
		return WithheldUnscoped
	}
}

// scrubbedErrorText is the scrubbed view of one persisted error text: the
// view every caller that is not admin-class reads, and the only one a
// companion key reads (D3).
func (s *Server) scrubbedErrorText(text string) string {
	out, _ := report.ScrubLines(text, s.liveLogs().scrub)
	return out
}

// respondLogScrubberUnavailable is the fail-closed answer of a route that
// would serve the scrubbed view and has no scrubber.
func respondLogScrubberUnavailable(w http.ResponseWriter) {
	respondError(w, http.StatusServiceUnavailable, "LOG_SCRUBBER_UNAVAILABLE",
		"the task log is unavailable to this caller: the log scrubber could not be built")
}

// heldTaskLog is the server's daemon-held task log, built from its own
// repositories and live subscriber.
func (s *Server) heldTaskLog() HeldTaskLog {
	h := HeldTaskLog{Tasks: s.taskRepo, Executions: s.executionRepo}
	if s.liveSub != nil {
		h.Live = s.liveSub
	}
	return h
}

func (s *Server) persistedTaskLogExcerpt(ctx context.Context, projectID, taskID string) (string, error) {
	return persistedTaskLogExcerpt(ctx, s.taskRepo, s.executionRepo, projectID, taskID)
}

// persistedTaskLogExcerpt is the newest execution's error, else its result
// message, else the task's last_error, each only when it belongs to
// projectID. One implementation for the companion task_logs tool and
// HeldTaskLog.
func persistedTaskLogExcerpt(ctx context.Context, tasks persistence.TaskRepository, execs persistence.ExecutionRepository, projectID, taskID string) (string, error) {
	if execs != nil {
		exec, err := execs.GetByTaskID(ctx, taskID)
		if err == nil && exec != nil && exec.ProjectID == projectID {
			if exec.ErrorMessage != nil && strings.TrimSpace(*exec.ErrorMessage) != "" {
				return *exec.ErrorMessage, nil
			}
			if msg := resultMessage(exec.Result); msg != "" {
				return msg, nil
			}
		}
	}
	if tasks != nil {
		task, err := tasks.Get(ctx, taskID)
		if err == nil && task != nil && task.ProjectID == projectID && task.LastError != nil && strings.TrimSpace(*task.LastError) != "" {
			return *task.LastError, nil
		}
	}
	return "", errNoHeldTaskLog
}

func resultMessage(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var result struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ""
	}
	if strings.TrimSpace(result.Message) != "" {
		return result.Message
	}
	if result.Status != "" {
		return "result status: " + result.Status
	}
	return ""
}

func trimLines(s string, max int) string {
	if max <= 0 {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= max {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-max:], "\n")
}

package api

// GitHub #814 (github-app-channel design, "Reading a large diff in parts"):
// GET /api/v1/projects/{p}/tasks/{t}/executions/{e}/diff-part?part=N serves
// part N of the masked diff this running execution's forge.fetch_diff step
// produced, to the per-task key of that task only. The reader
// (forge.DiffPart) re-proves the part with the detector before serving it.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/executor/handlers/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

var forgeDiffPartsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "vornik_forge_diff_parts_total",
	Help: "forge_diff_part calls by outcome: served, refused, or mask_failed (the rescan found a value; nothing served).",
}, []string{"outcome"})

func (s *Server) refuseDiffPart(w http.ResponseWriter, status int, code, msg string) {
	forgeDiffPartsTotal.WithLabelValues("refused").Inc()
	respondError(w, status, code, msg)
}

// ForgeDiffPart serves one part of a masked diff (see the file comment).
func (s *Server) ForgeDiffPart(w http.ResponseWriter, r *http.Request) {
	projectID, taskID, execID := extractProjectID(r), extractTaskID(r), extractExecutionID(r)
	// Only this running task's own per-task key: no project, warm or operator
	// key needs to read a review's diff parts.
	if bound, ok := s.runningAgentTaskKey(r); !ok || bound != taskID {
		s.refuseDiffPart(w, http.StatusForbidden, "FORBIDDEN", "forge_diff_part needs the per-task key of this running task")
		return
	}
	part, err := strconv.Atoi(r.URL.Query().Get("part"))
	if err != nil {
		s.refuseDiffPart(w, http.StatusBadRequest, "VALIDATION_ERROR", "part must be an integer of 2 or more")
		return
	}
	if s.executionRepo == nil {
		s.refuseDiffPart(w, http.StatusServiceUnavailable, "UNAVAILABLE", "executions are not available")
		return
	}
	exec, err := s.executionRepo.Get(r.Context(), execID)
	if err != nil || exec == nil || exec.TaskID != taskID || exec.ProjectID != projectID {
		s.refuseDiffPart(w, http.StatusNotFound, "NOT_FOUND", "no such execution of this task")
		return
	}
	if exec.Status != persistence.ExecutionStatusRunning {
		s.refuseDiffPart(w, http.StatusConflict, "NOT_RUNNING", "the execution is not running")
		return
	}
	if !forge.IsMaskedFetchStep(executionWorkflow(exec)) {
		s.refuseDiffPart(w, http.StatusConflict, "NOT_MASKED", "this execution's workflow has no masked forge.fetch_diff step")
		return
	}
	var state struct {
		StepResults map[string]json.RawMessage `json:"stepResults"`
	}
	if json.Unmarshal(exec.StateSnapshot, &state) != nil || len(state.StepResults[forge.FetchDiffStepID]) == 0 {
		s.refuseDiffPart(w, http.StatusConflict, "NOT_FETCHED", "the diff has not been fetched yet")
		return
	}
	text, _, err := forge.DiffPart(state.StepResults[forge.FetchDiffStepID], part, s.secretsDetector)
	switch {
	case errors.Is(err, forge.ErrDiffPartMask):
		forgeDiffPartsTotal.WithLabelValues("mask_failed").Inc()
		s.logger.Warn().Str("execution_id", execID).Int("part", part).Msg("forge_diff_part: the rescan found a value; nothing served")
		respondError(w, http.StatusConflict, "MASK_FAILED", err.Error())
		return
	case errors.Is(err, forge.ErrDiffPartRange):
		s.refuseDiffPart(w, http.StatusBadRequest, "NO_SUCH_PART", err.Error())
		return
	case err != nil:
		s.refuseDiffPart(w, http.StatusConflict, "NOT_SERVED", err.Error())
		return
	}
	forgeDiffPartsTotal.WithLabelValues("served").Inc()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

// executionWorkflow is the workflow the execution ran: its snapshot only.
// The registry's current definition is not a fallback (#814 review a38e M1):
// it may have gained mask_secrets after this execution ran unmasked.
func executionWorkflow(exec *persistence.Execution) *registry.Workflow {
	wf := &registry.Workflow{}
	if len(exec.WorkflowSnapshot) == 0 || json.Unmarshal(exec.WorkflowSnapshot, wf) != nil {
		return nil
	}
	return wf
}

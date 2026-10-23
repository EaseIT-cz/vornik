package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

// explainResponse is the JSON shape returned by the explain endpoint.
// Matches postmortem.RenderResult so the CLI can decode without a
// translation layer.
type explainResponse struct {
	Summary string                 `json:"summary"`
	Inputs  map[string]interface{} `json:"inputs"`
}

// ExplainTask handles POST /api/v1/projects/{projectID}/tasks/{taskID}/explain.
// Joins the task's failure context (LastError + class, step outcomes,
// recent tool calls, the daemon-held task log tail — GitHub #294: never
// podman logs) and renders an operator-friendly
// summary deterministically — no LLM call. The previous design called
// an LLM here; the determinism collapse moved the prose-summarisation
// to a static template + the failure-class playbook. Operators who
// want LLM-elaborated prose use the UI "Post-mortem" button which
// goes through postmortem.Explainer.Generate (persisted, billed).
func (s *Server) ExplainTask(w http.ResponseWriter, r *http.Request, projectID, taskID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		// GET is convenient for the UI and curl spelunking; POST is
		// the documented form. Both produce identical output and
		// the endpoint has no side effects.
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// GitHub #491: an unscoped caller that is not admin-class reads no
	// explain, decided before the task read, the renderer and the scrubber.
	access := s.errorTextAccess(r)
	if access == ErrorTextDenied {
		RespondProjectScopeRequired(w)
		return
	}
	if s.explainRenderer == nil {
		respondError(w, http.StatusServiceUnavailable, "EXPLAIN_NOT_CONFIGURED",
			"explain endpoint not wired; pass WithExplainRenderer at server construction")
		return
	}
	if projectID == "" || taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"projectId and taskId are both required")
		return
	}

	// Verify the task belongs to the named project so a typo doesn't
	// silently explain the wrong row.
	if s.taskRepo != nil {
		t, err := s.taskRepo.Get(r.Context(), taskID)
		if err == nil && t != nil && t.ProjectID != projectID {
			respondError(w, http.StatusNotFound, "NOT_FOUND",
				"task not found in project")
			return
		}
	}

	// GitHub #440: only an admin-class caller reads the operator view. Every
	// other caller renders with a copy whose log tail is the scrubbed held
	// log and whose free-text fields are scrubbed before they are cut, so the
	// summary built from them cannot quote a raw value (process-spawn-law
	// design, "Amendment 2026-10-21 (#439, #440)", C3 R2).
	renderer := s.explainRenderer
	if access != ErrorTextOperator {
		scrub := s.liveLogs().scrub
		held, herr := s.heldTaskLog().ScrubbedView(scrub)
		if herr != nil {
			respondLogScrubberUnavailable(w)
			return
		}
		cp := *s.explainRenderer
		cp.Scrub = scrub
		if cp.Logs != nil {
			cp.Logs = held
		}
		renderer = &cp
	}

	result, err := renderer.Render(r.Context(), taskID)
	if err != nil {
		// Structured-cause mapping: ErrNotFound / "not found" → 404,
		// anything else → 500 with the error text. The CLI surfaces
		// the message verbatim.
		if errors.Is(err, persistence.ErrNotFound) || strings.Contains(err.Error(), "not found") {
			respondError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, "EXPLAIN_FAILED", err.Error())
		return
	}
	// The summary is built from the scrubbed inputs; it passes the scrubber
	// once more so a field added to the template later is covered too
	// (code review review-20261021-0075 F2, as the explainer's second pass).
	if renderer.Scrub != nil {
		result.Summary, _ = report.ScrubLines(result.Summary, renderer.Scrub)
	}

	// Re-marshal Inputs through json so we don't bind the wire to the
	// concrete RenderedInputs struct — keeps the package boundary
	// clean if Inputs grows fields the API doesn't want to expose yet.
	inputsBytes, _ := json.Marshal(result.Inputs)
	var inputsMap map[string]interface{}
	_ = json.Unmarshal(inputsBytes, &inputsMap)
	resp := explainResponse{Summary: result.Summary, Inputs: inputsMap}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Warn().Err(err).Msg("explain endpoint: response encode failed")
	}
}

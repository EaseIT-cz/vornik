package api

// GitHub #809 (agent-benchmark design, Amendment 2026-11-18 (#809), the
// revision): the bench daemon's learning-study routes, all admin-gated and
// answering 404 on any daemon that is not a learning-study bench.
//
//	POST /api/v1/admin/bench/freeze        stop learning before the measured pass
//	GET  /api/v1/admin/bench/learned-state what the arm learned, and its hash
//	POST /api/v1/admin/bench/triggers      the bench trigger, for the harness's autopilot
//
// The autopilot's other steps are the shipped healing routes (generate,
// replay trial, promote); these add only what those lack.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/benchlearning"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// BenchTriggerMetric marks a bench trigger's row: the shipped trigger class
// with this metric name, so no schema changes. The autopilot and the learned
// state select on it.
const BenchTriggerMetric = "bench_absolute_failure_rate"

// WithBenchLearning wires the learning study's config and freeze gate. A nil
// gate (every daemon that is not a bench) leaves the routes answering 404.
func WithBenchLearning(cfg config.BenchConfig, gate *benchlearning.Gate) ServerOption {
	return func(s *Server) { s.benchConfig, s.benchGate = cfg, gate }
}

// benchLearnedState is the learned-state route's answer.
type benchLearnedState struct {
	TriggerEnabled bool                `json:"trigger_enabled"`
	Auto           bool                `json:"auto"`
	Frozen         bool                `json:"frozen"`
	FrozenAt       *time.Time          `json:"frozen_at,omitempty"`
	Instincts      []benchInstinct     `json:"instincts"`
	Promoted       []benchPromotedCand `json:"promoted"`
	SHA256         string              `json:"sha256"`
	// Candidates is every healing candidate with its trigger and status, for
	// the autopilot to follow its own trigger; it is not in the hash.
	Candidates []benchCandidate `json:"candidates"`
	// Triggers is every bench trigger with its workflow and status, so the
	// autopilot writes none while one is open; it is not in the hash.
	Triggers []benchTrigger `json:"triggers"`
}

type benchTrigger struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	Status     string `json:"status"`
}

type benchCandidate struct {
	ID        string `json:"id"`
	TriggerID string `json:"trigger_id"`
	Status    string `json:"status"`
}

type benchInstinct struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence"`
}

type benchPromotedCand struct {
	CandidateID string `json:"candidate_id"`
	WorkflowID  string `json:"workflow_id"`
	GenomeHash  string `json:"genome_hash"`
}

// benchRoute admits a request to a learning-study route: admin-gated, and
// 404 when this daemon is not a learning-study bench.
func (s *Server) benchRoute(w http.ResponseWriter, r *http.Request, method string) bool {
	if s.benchGate == nil {
		http.NotFound(w, r)
		return false
	}
	if r.Method != method {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use "+method)
		return false
	}
	return s.requireAdminGate(w, r)
}

// AdminBenchFreeze stops learning. It returns once any instinct tick in
// flight has finished, so the learned state read after it cannot move.
func (s *Server) AdminBenchFreeze(w http.ResponseWriter, r *http.Request) {
	if !s.benchRoute(w, r, http.MethodPost) {
		return
	}
	at, err := s.benchGate.Freeze()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "FREEZE_FAILED", err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"frozen_at": at})
}

// AdminBenchLearnedState reports what this arm has learned: every instinct by
// id, status and confidence, every promoted healing candidate, and a hash of
// both, so the harness can prove the measured pass ran on one treatment.
func (s *Server) AdminBenchLearnedState(w http.ResponseWriter, r *http.Request) {
	if !s.benchRoute(w, r, http.MethodGet) {
		return
	}
	st := benchLearnedState{TriggerEnabled: s.benchConfig.Learning.Trigger.Enabled, Auto: s.benchConfig.Learning.Auto,
		Instincts: []benchInstinct{}, Promoted: []benchPromotedCand{}, Candidates: []benchCandidate{}, Triggers: []benchTrigger{}}
	if at, frozen := s.benchGate.FrozenAt(); frozen {
		st.Frozen, st.FrozenAt = true, &at
	}
	if s.instinctRepo != nil {
		rows, err := s.instinctRepo.List(r.Context(), persistence.InstinctFilter{PageSize: 10000})
		if err != nil {
			respondError(w, http.StatusInternalServerError, "INSTINCTS_UNREADABLE", err.Error())
			return
		}
		for _, in := range rows {
			st.Instincts = append(st.Instincts, benchInstinct{ID: in.ID, Status: in.Status, Confidence: in.Confidence})
		}
	}
	if s.healingCandidateRepo != nil {
		rows, err := s.healingCandidateRepo.List(r.Context(), persistence.HealingCandidateListFilter{PageSize: 10000})
		if err != nil {
			respondError(w, http.StatusInternalServerError, "CANDIDATES_UNREADABLE", err.Error())
			return
		}
		for _, c := range rows {
			st.Candidates = append(st.Candidates, benchCandidate{ID: c.ID, TriggerID: c.TriggerID, Status: string(c.Status)})
			if c.Status == persistence.HealingCandidatePromoted {
				st.Promoted = append(st.Promoted, benchPromotedCand{CandidateID: c.ID, WorkflowID: c.WorkflowID, GenomeHash: c.CandidateGenomeHash})
			}
		}
	}
	if s.healingTriggerRepo != nil {
		rows, err := s.healingTriggerRepo.List(r.Context(), persistence.HealingTriggerListFilter{PageSize: 10000})
		if err != nil {
			respondError(w, http.StatusInternalServerError, "TRIGGERS_UNREADABLE", err.Error())
			return
		}
		for _, t := range rows {
			if t.MetricName == BenchTriggerMetric {
				st.Triggers = append(st.Triggers, benchTrigger{ID: t.ID, WorkflowID: t.WorkflowID, Status: string(t.Status)})
			}
		}
	}
	st.SHA256 = learnedStateHash(st)
	respondJSON(w, http.StatusOK, st)
}

// learnedStateHash is over the sorted instinct and promotion lines.
func learnedStateHash(st benchLearnedState) string {
	var lines []string
	for _, in := range st.Instincts {
		lines = append(lines, fmt.Sprintf("instinct %s %s %.6f", in.ID, in.Status, in.Confidence))
	}
	for _, c := range st.Promoted {
		lines = append(lines, fmt.Sprintf("promoted %s %s %s", c.CandidateID, c.WorkflowID, c.GenomeHash))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// benchTriggerRequest is the harness autopilot's trigger.
type benchTriggerRequest struct {
	ProjectID            string   `json:"project_id"`
	WorkflowID           string   `json:"workflow_id"`
	FailureRate          float64  `json:"failure_rate"`
	Threshold            float64  `json:"threshold"`
	Runs                 int      `json:"runs"`
	EvidenceExecutionIDs []string `json:"evidence_execution_ids"`
}

// AdminBenchCreateTrigger writes a bench trigger: only in an arm with the
// bench trigger on, and never once learning is frozen.
func (s *Server) AdminBenchCreateTrigger(w http.ResponseWriter, r *http.Request) {
	if !s.benchRoute(w, r, http.MethodPost) {
		return
	}
	if !s.benchConfig.Learning.Trigger.Enabled {
		respondError(w, http.StatusConflict, "BENCH_TRIGGER_OFF", "this arm does not run the bench trigger (bench.learning.trigger.enabled is false)")
		return
	}
	if _, frozen := s.benchGate.FrozenAt(); frozen {
		respondError(w, http.StatusConflict, "LEARNING_FROZEN", "learning is frozen for the measured pass; no trigger is written")
		return
	}
	if s.healingTriggerRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "TRIGGERS_NOT_WIRED", "the healing trigger store is not wired")
		return
	}
	var req benchTriggerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "body must be JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ProjectID) == "" || strings.TrimSpace(req.WorkflowID) == "" || len(req.EvidenceExecutionIDs) == 0 {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "project_id, workflow_id and evidence_execution_ids are required")
		return
	}
	now := time.Now().UTC()
	t := &persistence.HealingTrigger{
		ProjectID: req.ProjectID, WorkflowID: req.WorkflowID,
		TriggerClass:    persistence.HealingTriggerFailureRateSpike,
		MetricName:      BenchTriggerMetric,
		ComparisonValue: req.FailureRate, ThresholdValue: req.Threshold,
		BaselineStart: now, BaselineEnd: now, ComparisonStart: now, ComparisonEnd: now,
		EvidenceExecutionIDs: req.EvidenceExecutionIDs,
		Status:               persistence.HealingTriggerStatusOpen,
		CreatedAt:            now,
	}
	if err := s.healingTriggerRepo.Insert(r.Context(), t); err != nil {
		respondError(w, http.StatusInternalServerError, "TRIGGER_NOT_WRITTEN", err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"trigger_id": t.ID})
}

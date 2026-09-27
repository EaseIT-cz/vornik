package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// ExecutionQualityScore is the durable, one-row-per-execution quality verdict.
// Score is nil exactly when there is no measurement: status not_applicable or
// unscorable (ExecutionQualityScoreUnmeasured).
type ExecutionQualityScore struct {
	ProjectID        string          `json:"project_id"`
	TaskID           string          `json:"task_id"`
	ExecutionID      string          `json:"execution_id"`
	WorkflowID       string          `json:"workflow_id"`
	WorkflowRevision string          `json:"workflow_revision"`
	ScorerVersion    string          `json:"scorer_version"`
	ScoringPolicySHA string          `json:"scoring_policy_sha"`
	Kind             string          `json:"kind"`
	Status           string          `json:"status"`
	Score            *float64        `json:"score,omitempty"`
	PassedCaseCount  int             `json:"passed_case_count"`
	PinnedCaseCount  int             `json:"pinned_case_count"`
	Diagnostic       string          `json:"diagnostic,omitempty"`
	CaseEvidence     json.RawMessage `json:"case_evidence"`
	RecordedAt       time.Time       `json:"recorded_at"`
}

// ExecutionQualityScoreFilter scopes and paginates operator score queries.
type ExecutionQualityScoreFilter struct {
	ProjectIDs  []string
	TaskID      string
	ExecutionID string
	WorkflowID  string
	Statuses    []string
	Since       *time.Time
	MaxScore    *float64
	PageSize    int
	Offset      int
}

// ExecutionQualityPendingStats describes terminal executions awaiting publication.
type ExecutionQualityPendingStats struct {
	Count    int64      `json:"count"`
	OldestAt *time.Time `json:"oldest_at,omitempty"`
}

// ExecutionQualityScoreRepository persists scores and finds publication gaps.
type ExecutionQualityScoreRepository interface {
	Upsert(ctx context.Context, score *ExecutionQualityScore) error
	GetByExecution(ctx context.Context, executionID string) (*ExecutionQualityScore, error)
	List(ctx context.Context, filter ExecutionQualityScoreFilter) ([]*ExecutionQualityScore, error)
	ListPendingTerminal(ctx context.Context, limit int) ([]*Execution, error)
	PendingTerminalStats(ctx context.Context, projectIDs []string) (ExecutionQualityPendingStats, error)
}

// ExecutionQualityStatuses is the ONE list of durable statuses. The validator
// and the API list filter read it; the Postgres and SQLite CHECKs spell the
// same five values (agent-quality-benchmark design, amendment 2026-09-26).
// A status the scorer learns must be added here, or its executions cannot
// publish; internal/quality's AST test fails first.
var ExecutionQualityStatuses = []string{"scored", "missing_contract", "invalid_evidence", "not_applicable", "unscorable"}

// ExecutionQualityScoreUnmeasured reports the statuses whose row has no
// score: not_applicable (no contract) and unscorable (the contract cannot
// evaluate this execution). A NULL keeps them out of any AVG(score).
func ExecutionQualityScoreUnmeasured(status string) bool {
	return status == "not_applicable" || status == "unscorable"
}

// KnownExecutionQualityStatus reports whether status is a durable status.
func KnownExecutionQualityStatus(status string) bool {
	for _, s := range ExecutionQualityStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// ErrInvalidQualityScore marks a row that can never be written as it stands,
// as opposed to a write that failed and may succeed on retry. The publisher
// stops retrying an execution whose row is rejected with it.
var ErrInvalidQualityScore = errors.New("invalid execution quality score")

// ValidateExecutionQualityScore enforces the durable score-row invariants.
// Every rejection wraps ErrInvalidQualityScore.
func ValidateExecutionQualityScore(s *ExecutionQualityScore) error {
	if err := validateExecutionQualityScore(s); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQualityScore, err)
	}
	return nil
}

func validateExecutionQualityScore(s *ExecutionQualityScore) error {
	if s == nil {
		return fmt.Errorf("execution quality score is nil")
	}
	if s.ProjectID == "" || s.TaskID == "" || s.ExecutionID == "" || s.WorkflowID == "" {
		return fmt.Errorf("execution quality score requires project_id, task_id, execution_id, and workflow_id")
	}
	switch {
	case !KnownExecutionQualityStatus(s.Status):
		return fmt.Errorf("unknown execution quality score status %q", s.Status)
	case ExecutionQualityScoreUnmeasured(s.Status) && s.Score != nil:
		return fmt.Errorf("%s execution quality score must not be numeric", s.Status)
	case !ExecutionQualityScoreUnmeasured(s.Status) && s.Score == nil:
		return fmt.Errorf("execution quality score status %q requires a numeric score", s.Status)
	}
	if s.Score != nil && (math.IsNaN(*s.Score) || math.IsInf(*s.Score, 0) || *s.Score < 0 || *s.Score > 1) {
		return fmt.Errorf("execution quality score must be finite and within [0,1]")
	}
	if s.PassedCaseCount < 0 || s.PinnedCaseCount < 0 || s.PassedCaseCount > s.PinnedCaseCount {
		return fmt.Errorf("invalid execution quality case counts %d/%d", s.PassedCaseCount, s.PinnedCaseCount)
	}
	return nil
}

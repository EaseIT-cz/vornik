package persistence

import (
	"context"
	"time"
)

// ShadowDecision is one candidate on one tick, as the rule engine and the
// strategist each decided it (GitHub #598 step 1; trading entry-policy
// design, Amendment 2026-10-31 §2 C). Measurement only.
type ShadowDecision struct {
	ExecutionID  string
	Symbol       string
	Intent       string
	ProjectID    string
	ObservedAt   time.Time
	EngineStatus string
	EngineGate   string
	ModelStatus  string // "" when the model's funnel had no row for it
	ModelGate    string // normalised
	Agree        bool
}

// ShadowDecisionRepository stores the shadow record.
type ShadowDecisionRepository interface {
	// Record writes one tick's rows; a repeat of (execution, symbol,
	// intent) is a no-op.
	Record(ctx context.Context, rows []ShadowDecision) error
	// Since returns a project's rows observed at or after since, oldest
	// first.
	Since(ctx context.Context, projectID string, since time.Time) ([]ShadowDecision, error)
}

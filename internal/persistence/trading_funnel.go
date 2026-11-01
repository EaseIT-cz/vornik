package persistence

import (
	"context"
	"encoding/json"
	"time"
)

// FunnelCandidate is one row of `trading_funnel_candidates`: one entry of
// the strategist's entry funnel on one tick, with its forward outcome once
// known (GitHub #596; trading entry-policy design, Amendment 2026-10-31).
// Measurement only. The key is (ExecutionID, Symbol, Intent); the first
// write wins.
type FunnelCandidate struct {
	ExecutionID string
	Symbol      string
	Intent      string
	TaskID      string
	ProjectID   string
	StepID      string
	ObservedAt  time.Time
	Status      string
	Gate        string // normalised
	GateRaw     string // as the model wrote it
	Reason      string
	Inputs      json.RawMessage
	RefClose    *float64
	RefATR      *float64
	// Outcomes, nil until known: index 0, 1, 2 are the 1-, 3- and
	// 5-session horizons.
	Ret [3]*float64
	MFE [3]*float64
	MAE [3]*float64
}

// FunnelHorizons are the forward-outcome horizons, in completed sessions.
var FunnelHorizons = [3]int{1, 3, 5}

// FunnelRepository stores the entry funnel and its forward outcomes.
type FunnelRepository interface {
	// Record writes the candidates of one tick; a repeat of a key is a
	// no-op.
	Record(ctx context.Context, rows []FunnelCandidate) error
	// Pending returns a project's candidates for symbol that have a
	// reference close and an outcome still unknown, oldest first.
	Pending(ctx context.Context, projectID, symbol string) ([]FunnelCandidate, error)
	// SetOutcome writes horizon i's outcome (i indexes FunnelHorizons) for
	// one candidate, only while it is unknown: an outcome is written once.
	// A nil ATR measure stays NULL.
	SetOutcome(ctx context.Context, executionID, symbol, intent string, i int, ret float64, mfe, mae *float64) error
	// Since returns a project's candidates observed at or after since,
	// oldest first.
	Since(ctx context.Context, projectID string, since time.Time) ([]FunnelCandidate, error)
}

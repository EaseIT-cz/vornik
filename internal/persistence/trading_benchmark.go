package persistence

import (
	"context"
	"time"
)

// BenchmarkClose is one completed session's close of a benchmark symbol
// (SPY), recorded from bars the daemon already fetches (GitHub #597;
// trading-performance-tracker design, Amendment 2026-10-31). Session is the
// New York session date at midnight UTC.
type BenchmarkClose struct {
	Symbol  string
	Session time.Time
	Close   float64
}

// BenchmarkCloseRepository stores benchmark closes. Not project-scoped: a
// symbol's close is the same for every project.
type BenchmarkCloseRepository interface {
	// Record writes closes; a repeat of (symbol, session) is a no-op.
	Record(ctx context.Context, closes []BenchmarkClose) error
	// Range returns a symbol's closes with from <= session <= to, oldest
	// first.
	Range(ctx context.Context, symbol string, from, to time.Time) ([]BenchmarkClose, error)
}

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// BenchmarkCloseRepository is the SQLite store of benchmark closes
// (mirrors postgres migration 245; GitHub #597).
type BenchmarkCloseRepository struct {
	db *sql.DB
}

// NewBenchmarkCloseRepository builds the repository.
func NewBenchmarkCloseRepository(db *sql.DB) *BenchmarkCloseRepository {
	return &BenchmarkCloseRepository{db: db}
}

var _ persistence.BenchmarkCloseRepository = (*BenchmarkCloseRepository)(nil)

// Record writes closes; the first write of (symbol, session) wins.
func (r *BenchmarkCloseRepository) Record(ctx context.Context, closes []persistence.BenchmarkClose) error {
	for _, c := range closes {
		if _, err := r.db.ExecContext(ctx,
			`INSERT INTO trading_benchmark_closes (symbol, session, close) VALUES (?, ?, ?)
			 ON CONFLICT (symbol, session) DO NOTHING`,
			c.Symbol, c.Session.UTC().Format(time.DateOnly), c.Close); err != nil {
			return fmt.Errorf("sqlite: trading_benchmark_closes insert: %w", err)
		}
	}
	return nil
}

// Range returns a symbol's closes in [from, to], oldest first.
func (r *BenchmarkCloseRepository) Range(ctx context.Context, symbol string, from, to time.Time) ([]persistence.BenchmarkClose, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT session, close FROM trading_benchmark_closes
		  WHERE symbol = ? AND session >= ? AND session <= ? ORDER BY session`,
		symbol, from.UTC().Format(time.DateOnly), to.UTC().Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("sqlite: trading_benchmark_closes range: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []persistence.BenchmarkClose
	for rows.Next() {
		var (
			day string
			c   persistence.BenchmarkClose
		)
		if err := rows.Scan(&day, &c.Close); err != nil {
			return nil, fmt.Errorf("sqlite: trading_benchmark_closes scan: %w", err)
		}
		if c.Session, err = time.Parse(time.DateOnly, day); err != nil {
			return nil, fmt.Errorf("sqlite: trading_benchmark_closes session %q: %w", day, err)
		}
		c.Symbol = symbol
		out = append(out, c)
	}
	return out, rows.Err()
}

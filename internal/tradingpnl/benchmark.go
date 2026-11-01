package tradingpnl

import (
	"time"

	"vornik.io/vornik/internal/persistence"
)

// VsRow is one session of the book against a benchmark (GitHub #597).
// Percentages are in percent. The book is marked to market; the benchmark
// is price return without dividends; the two are a trend line, not an
// attribution (trading-performance-tracker design, Amendment 2026-10-31).
type VsRow struct {
	Day        time.Time
	BookPct    float64 // equity change on the strategy capital
	BenchPct   float64 // benchmark close to close
	CumDiffPct float64 // running sum of BookPct - BenchPct
}

// VsBenchmark lines the book's daily equity change, as a share of capital,
// up against the benchmark's close-to-close change, on each session that
// has both and a previous session with both. capital <= 0 returns nil.
func VsBenchmark(equity []DailyPoint, closes []persistence.BenchmarkClose, capital float64) []VsRow {
	if capital <= 0 {
		return nil
	}
	eq := make(map[time.Time]float64, len(equity))
	for _, p := range equity {
		eq[p.Day.UTC().Truncate(24*time.Hour)] = p.EquityUSD
	}
	var rows []VsRow
	var cum float64
	for i := 1; i < len(closes); i++ {
		prev, cur := closes[i-1], closes[i]
		e0, ok0 := eq[prev.Session]
		e1, ok1 := eq[cur.Session]
		if !ok0 || !ok1 || prev.Close <= 0 {
			continue
		}
		book := 100 * (e1 - e0) / capital
		bench := 100 * (cur.Close/prev.Close - 1)
		cum += book - bench
		rows = append(rows, VsRow{Day: cur.Session, BookPct: book, BenchPct: bench, CumDiffPct: cum})
	}
	return rows
}

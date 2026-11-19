package tradingpnl

// GitHub #825: the dashboard shows each exit fill's realized P&L, so a closed
// lot names the fill that closed it. One exit across two lots gives two
// trades with the same exit fill.

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestPairRoundTrips825_ExitFillIDNamesTheClosingFill(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	fills := []*persistence.TradingFill{
		{ID: "f-buy-1", OrderID: "o-buy-1", Symbol: "AAPL", Qty: 5, Price: 100, FilledAt: t0},
		{ID: "f-buy-2", OrderID: "o-buy-2", Symbol: "AAPL", Qty: 5, Price: 110, FilledAt: t0.Add(time.Hour)},
		{ID: "f-sell", OrderID: "o-sell", Symbol: "AAPL", Qty: 10, Price: 120, FilledAt: t0.Add(48 * time.Hour)},
	}
	side := map[string]string{"o-buy-1": "BUY", "o-buy-2": "BUY", "o-sell": "SELL"}
	trades := PairRoundTrips(fills, side)
	if len(trades) != 2 {
		t.Fatalf("trades = %d, want 2 (one exit across two lots)", len(trades))
	}
	sum := 0.0
	for _, tr := range trades {
		if tr.ExitFillID != "f-sell" {
			t.Errorf("trade from %s: ExitFillID = %q, want f-sell", tr.EntryOrderID, tr.ExitFillID)
		}
		sum += tr.RealizedUSD
	}
	if sum != 150 { // (120-100)*5 + (120-110)*5
		t.Fatalf("realized over the exit fill = %v, want 150", sum)
	}
}

func TestPairRoundTrips825_ShortCoverNamesItsBuy(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	fills := []*persistence.TradingFill{
		{ID: "f-short", OrderID: "o-s", Symbol: "TSLA", Qty: 2, Price: 200, FilledAt: t0},
		{ID: "f-cover", OrderID: "o-c", Symbol: "TSLA", Qty: 2, Price: 190, FilledAt: t0.Add(time.Hour)},
	}
	trades := PairRoundTrips(fills, map[string]string{"o-s": "SELL", "o-c": "BUY"})
	if len(trades) != 1 || trades[0].ExitFillID != "f-cover" || trades[0].RealizedUSD != 20 {
		t.Fatalf("trades = %+v", trades)
	}
}

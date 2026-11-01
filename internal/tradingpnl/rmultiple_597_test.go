package tradingpnl

import (
	"math"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #597 (trading-performance-tracker design, Amendment 2026-10-31):
// results in R-multiples, one per entry order, from the entry fills and the
// bracket's "_stop" child. Measurement only.

func at(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }

func fp(v float64) *float64 { return &v }

func order(id, key, sym, action, typ string, qty, filled float64, stop *float64) *persistence.TradingOrder {
	return &persistence.TradingOrder{ID: id, IdempotencyKey: key, Symbol: sym, Action: action, OrderType: typ,
		Qty: qty, FilledQty: filled, StopPrice: stop}
}

func fill(id, orderID, sym string, qty, px float64, t time.Time) *persistence.TradingFill {
	return &persistence.TradingFill{ID: id, OrderID: orderID, Symbol: sym, Qty: qty, Price: px, FilledAt: t}
}

func TestPairRoundTrips_SetsEntryOrderID_597(t *testing.T) {
	trades := PairRoundTrips([]*persistence.TradingFill{
		fill("f1", "o1", "AAPL", 10, 100, at(20, 14)),
		fill("f2", "o2", "AAPL", 10, 110, at(21, 14)),
	}, map[string]string{"o1": "BUY", "o2": "SELL"})
	if len(trades) != 1 || trades[0].EntryOrderID != "o1" {
		t.Fatalf("trades %+v", trades)
	}
}

func rFixture() ([]*persistence.TradingFill, []*persistence.TradingOrder) {
	orders := []*persistence.TradingOrder{
		// A: +2R winner. Entry 100, stop 95, 10 shares: risk 50; exit 110: +100.
		order("A", "kA", "AAPL", "BUY", "LMT", 10, 10, nil), order("As", "kA_stop", "AAPL", "SELL", "STP", 10, 0, fp(95)), order("Ax", "kAx", "AAPL", "SELL", "LMT", 10, 10, nil),
		// B: -1R loser. Entry 50, stop 48, 25 shares: risk 50; exit 48: -50.
		order("B", "kB", "MSFT", "BUY", "LMT", 25, 25, nil), order("Bs", "kB_stop", "MSFT", "SELL", "STP", 25, 25, fp(48)),
		// C: two fills (5@100, 5@102), stop 96, exited in two lots at 106: risk 5*4+5*6=50, P&L 5*6+5*4=50 => +1R, scored once.
		order("C", "kC", "SHEL", "BUY", "LMT", 10, 10, nil), order("Cs", "kC_stop", "SHEL", "SELL", "STP", 10, 0, fp(96)), order("Cx", "kCx", "SHEL", "SELL", "LMT", 10, 10, nil),
		// D: partly closed (10 filled, 4 sold): open.
		order("D", "kD", "AZN", "BUY", "LMT", 10, 10, nil), order("Ds", "kD_stop", "AZN", "SELL", "STP", 10, 0, fp(60)), order("Dx", "kDx", "AZN", "SELL", "LMT", 4, 4, nil),
		// E: trailing child; F: no child; G: stop above entry.
		order("E", "kE", "JPM", "BUY", "LMT", 1, 1, nil), order("Es", "kE_stop", "JPM", "SELL", "TRAIL", 1, 0, nil), order("Ex", "kEx", "JPM", "SELL", "LMT", 1, 1, nil),
		order("F", "kF", "ASML", "BUY", "LMT", 1, 1, nil), order("Fx", "kFx", "ASML", "SELL", "LMT", 1, 1, nil),
		order("G", "kG", "SAP", "BUY", "LMT", 1, 1, nil), order("Gs", "kG_stop", "SAP", "SELL", "STP", 1, 0, fp(300)), order("Gx", "kGx", "SAP", "SELL", "LMT", 1, 1, nil),
		// H: before the reliable-ledger date.
		order("H", "kH", "NVDA", "BUY", "LMT", 1, 1, nil), order("Hs", "kH_stop", "NVDA", "SELL", "STP", 1, 0, fp(90)), order("Hx", "kHx", "NVDA", "SELL", "LMT", 1, 1, nil),
	}
	early := time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC)
	fills := []*persistence.TradingFill{
		fill("a1", "A", "AAPL", 10, 100, at(20, 14)), fill("a2", "Ax", "AAPL", 10, 110, at(22, 14)),
		fill("b1", "B", "MSFT", 25, 50, at(20, 14)), fill("b2", "Bs", "MSFT", 25, 48, at(21, 14)),
		fill("c1", "C", "SHEL", 5, 100, at(20, 14)), fill("c2", "C", "SHEL", 5, 102, at(20, 15)),
		fill("c3", "Cx", "SHEL", 5, 106, at(23, 14)), fill("c4", "Cx", "SHEL", 5, 106, at(23, 15)),
		fill("d1", "D", "AZN", 10, 70, at(20, 14)), fill("d2", "Dx", "AZN", 4, 72, at(21, 14)),
		fill("e1", "E", "JPM", 1, 200, at(20, 14)), fill("e2", "Ex", "JPM", 1, 210, at(21, 14)),
		fill("f1", "F", "ASML", 1, 700, at(20, 14)), fill("f2", "Fx", "ASML", 1, 690, at(21, 14)),
		fill("g1", "G", "SAP", 1, 250, at(20, 14)), fill("g2", "Gx", "SAP", 1, 260, at(21, 14)),
		fill("h1", "H", "NVDA", 1, 100, early), fill("h2", "Hx", "NVDA", 1, 105, early.Add(time.Hour)),
	}
	return fills, orders
}

func TestEntryResults_597(t *testing.T) {
	fills, orders := rFixture()
	side := map[string]string{}
	for _, o := range orders {
		side[o.ID] = o.Action
	}
	res := EntryResults(PairRoundTrips(fills, side), orders)
	by := map[string]EntryResult{}
	for _, r := range res {
		by[r.EntryOrderID] = r
	}
	wantR := map[string]float64{"A": 2, "B": -1, "C": 1}
	for id, w := range wantR {
		if by[id].Status != EntryScored || by[id].R == nil || math.Abs(*by[id].R-w) > 1e-9 {
			t.Errorf("%s: %+v, want R %v", id, by[id], w)
		}
	}
	if by["D"].Status != EntryOpen {
		t.Errorf("D partly closed must be open: %+v", by["D"])
	}
	for id, reason := range map[string]string{"E": "trailing_stop", "F": "no_stop_child", "G": "nonpositive_risk"} {
		if by[id].Status != EntryUnknownRisk || by[id].Reason != reason || by[id].R != nil {
			t.Errorf("%s: %+v, want unknown risk %s", id, by[id], reason)
		}
	}
	if by["H"].Status != EntryExcluded {
		t.Errorf("H before the reliable ledger must be excluded: %+v", by["H"])
	}
}

func TestRStats_597(t *testing.T) {
	fills, orders := rFixture()
	side := map[string]string{}
	for _, o := range orders {
		side[o.ID] = o.Action
	}
	s := RStats(EntryResults(PairRoundTrips(fills, side), orders), at(1, 0), at(31, 0))
	if s.Scored != 3 || s.Open != 1 || s.Excluded != 0 || s.Unknown["trailing_stop"] != 1 || s.Unknown["no_stop_child"] != 1 || s.Unknown["nonpositive_risk"] != 1 {
		t.Fatalf("counts %+v", s)
	}
	if math.Abs(s.MeanR-2.0/3) > 1e-9 || s.MedianR != 1 || math.Abs(s.WinRatePct-200.0/3) > 1e-9 {
		t.Fatalf("mean %v median %v win %v", s.MeanR, s.MedianR, s.WinRatePct)
	}
	if s.AvgWinR != 1.5 || s.AvgLossR != -1 || s.ProfitFactorR == nil || *s.ProfitFactorR != 3 {
		t.Fatalf("avg win %v loss %v pf %v", s.AvgWinR, s.AvgLossR, s.ProfitFactorR)
	}
	if s.ProfitFactorUSD == nil || math.Abs(*s.ProfitFactorUSD-3) > 1e-9 {
		t.Fatalf("pf usd %v", s.ProfitFactorUSD)
	}
	// Only winners: no profit factor, never infinity. Even count: median of the middle two.
	w := RStats([]EntryResult{{Status: EntryScored, R: fp(1), RealizedUSD: 50, ExitAt: at(20, 0)}, {Status: EntryScored, R: fp(3), RealizedUSD: 150, ExitAt: at(20, 0)}}, at(1, 0), at(31, 0))
	if w.ProfitFactorR != nil || w.ProfitFactorUSD != nil || w.MedianR != 2 {
		t.Fatalf("no losses: %+v", w)
	}
}

package tradingpnl

import (
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The entry signal, summarized (GitHub #596 part 2; trading entry-policy
// design, Amendment 2026-10-31). A good signal shows proposed candidates
// beating rejected ones, independent of fills. Every number carries its
// sample; nothing here is a significance test.

// FunnelGroup is one group's forward outcomes. Index 0, 1, 2 are the 1-, 3-
// and 5-session horizons; Known counts the candidates whose outcome is
// known at that horizon, and means are over those only.
type FunnelGroup struct {
	Name       string
	N          int
	Known      [3]int
	MeanRet    [3]float64 // fraction, e.g. 0.01 = +1%
	TouchedATR [3]float64 // % of known whose MFE reached +1 ATR
}

// FunnelSummary is the report over a set of candidates.
type FunnelSummary struct {
	Candidates     int
	Sessions       int
	Pending        int // candidates with any horizon still unknown
	OtherSpellings []string
	Groups         []FunnelGroup // "proposed" first, then "rejected: <gate>" by size
}

// FunnelReport groups candidates as proposed or by the gate that rejected
// them, and averages their known forward outcomes.
func FunnelReport(rows []persistence.FunnelCandidate) FunnelSummary {
	var s FunnelSummary
	sessions := map[time.Time]bool{}
	others := map[string]bool{}
	groups := map[string]*funnelAcc{}
	for _, c := range rows {
		s.Candidates++
		sessions[c.ObservedAt.UTC().Truncate(24*time.Hour)] = true
		if c.Gate == "other" && c.GateRaw != "" {
			others[c.GateRaw] = true
		}
		name := funnelGroupName(c)
		a := groups[name]
		if a == nil {
			a = &funnelAcc{g: FunnelGroup{Name: name}}
			groups[name] = a
		}
		if a.add(c) {
			s.Pending++
		}
	}
	s.Sessions = len(sessions)
	for o := range others {
		s.OtherSpellings = append(s.OtherSpellings, o)
	}
	sort.Strings(s.OtherSpellings)
	for _, a := range groups {
		for i := range a.g.Known {
			if a.g.Known[i] > 0 {
				a.g.MeanRet[i] = a.sum[i] / float64(a.g.Known[i])
				a.g.TouchedATR[i] = 100 * a.touched[i] / float64(a.g.Known[i])
			}
		}
		s.Groups = append(s.Groups, a.g)
	}
	sort.Slice(s.Groups, func(i, j int) bool {
		gi, gj := s.Groups[i], s.Groups[j]
		if (gi.Name == "proposed") != (gj.Name == "proposed") {
			return gi.Name == "proposed"
		}
		if gi.N != gj.N {
			return gi.N > gj.N
		}
		return gi.Name < gj.Name
	})
	return s
}

// funnelAcc accumulates one group's outcomes.
type funnelAcc struct {
	g            FunnelGroup
	sum, touched [3]float64
}

// add counts one candidate and reports whether any of its horizons is
// still pending.
func (a *funnelAcc) add(c persistence.FunnelCandidate) (pending bool) {
	a.g.N++
	for i := range persistence.FunnelHorizons {
		if c.Ret[i] == nil {
			pending = true
			continue
		}
		a.g.Known[i]++
		a.sum[i] += *c.Ret[i]
		if c.MFE[i] != nil && *c.MFE[i] >= 1 {
			a.touched[i]++
		}
	}
	return pending
}

// funnelGroupName is "proposed", or "<status>: <gate>" ("rejected: regime").
func funnelGroupName(c persistence.FunnelCandidate) string {
	if c.Status == "proposed" {
		return "proposed"
	}
	return c.Status + ": " + c.Gate
}

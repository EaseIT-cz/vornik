package tradingpnl

import (
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The rule engine's shadow parity (GitHub #598 step 1; trading entry-policy
// design, Amendment 2026-10-31 §2 D). Agree means both decided the same
// status; SameGate counts the agreed rejections that also name the same
// gate.

// ShadowPair is one (engine, model) disagreement pattern.
type ShadowPair struct {
	Engine, Model string // "status: gate"
	Count         int
}

// ShadowSummary is the parity report over a window.
type ShadowSummary struct {
	Rows, Agree, SameGate, InsufficientInputs, Sessions int
	Pairs                                               []ShadowPair
	Disagreements                                       []persistence.ShadowDecision // newest first, at most 10
}

// ShadowReport summarizes shadow rows.
func ShadowReport(rows []persistence.ShadowDecision) ShadowSummary {
	var s ShadowSummary
	sessions := map[time.Time]bool{}
	pairs := map[[2]string]int{}
	var dis []persistence.ShadowDecision
	for _, r := range rows {
		s.Rows++
		sessions[r.ObservedAt.UTC().Truncate(24*time.Hour)] = true
		if r.EngineGate == "insufficient_inputs" {
			s.InsufficientInputs++
		}
		if r.Agree {
			s.Agree++
			if r.EngineGate == r.ModelGate {
				s.SameGate++
			}
			continue
		}
		model := r.ModelStatus + ": " + r.ModelGate
		if r.ModelStatus == "" {
			model = "no row"
		}
		pairs[[2]string{r.EngineStatus + ": " + r.EngineGate, model}]++
		dis = append(dis, r)
	}
	s.Sessions = len(sessions)
	for k, n := range pairs {
		s.Pairs = append(s.Pairs, ShadowPair{Engine: k[0], Model: k[1], Count: n})
	}
	sort.Slice(s.Pairs, func(i, j int) bool {
		if s.Pairs[i].Count != s.Pairs[j].Count {
			return s.Pairs[i].Count > s.Pairs[j].Count
		}
		return s.Pairs[i].Engine+s.Pairs[i].Model < s.Pairs[j].Engine+s.Pairs[j].Model
	})
	sort.SliceStable(dis, func(i, j int) bool { return dis[i].ObservedAt.After(dis[j].ObservedAt) })
	if len(dis) > 10 {
		dis = dis[:10]
	}
	s.Disagreements = dis
	return s
}

package trading

import (
	"strings"
	"testing"
)

// GitHub #716 (2026-11-10, found at production: no new entry since
// 2026-09-09 because one net-negative regime component of six blocked every
// long). block_long_at_score lets a project block longs only at a RISK_OFF
// score of -2; -1 keeps today's label check.

func score(n int) *int { return &n }

func riskOffLong(s *int) EntryProposal {
	return EntryProposal{Symbol: "AAPL", Side: "long", Region: "us", RegimeLabel: "RISK_OFF",
		RegimeScore: s, Total: 4, ComponentCount: 6, HasScores: true}
}

func TestEvaluateEntry_BlockAtMinusOneIsTheLabelCheck_716(t *testing.T) {
	c := cfg()
	c.BlockLongAtScore = -1
	for _, s := range []*int{nil, score(-1), score(-2), score(0)} {
		if ok, r := EvaluateEntry(riskOffLong(s), c); ok || r != "risk_off_long" {
			t.Fatalf("score %v at -1: want risk_off_long, got ok=%v r=%q", s, ok, r)
		}
	}
	// The zero value of the config is -1 too (every caller that predates #716).
	c.BlockLongAtScore = 0
	if ok, _ := EvaluateEntry(riskOffLong(score(-1)), c); ok {
		t.Fatal("unset threshold must keep today's block")
	}
}

func TestEvaluateEntry_BlockAtMinusTwo_716(t *testing.T) {
	c := cfg()
	c.BlockLongAtScore = -2
	if ok, r := EvaluateEntry(riskOffLong(score(-1)), c); !ok {
		t.Fatalf("score -1 at -2 must pass, got %q", r)
	}
	if ok, r := EvaluateEntry(riskOffLong(score(-2)), c); ok || r != "risk_off_long" {
		t.Fatalf("score -2 at -2 must block, got ok=%v r=%q", ok, r)
	}
	if ok, r := EvaluateEntry(riskOffLong(nil), c); ok || r != "risk_off_long" {
		t.Fatalf("missing score at -2 must fail closed, got ok=%v r=%q", ok, r)
	}
	for _, s := range []int{0, 1} {
		if ok, _ := EvaluateEntry(riskOffLong(score(s)), c); ok {
			t.Fatalf("a RISK_OFF label with score %d is malformed and must block at -2", s)
		}
	}
	short := riskOffLong(score(-2))
	short.Side = "short"
	if ok, r := EvaluateEntry(short, c); !ok {
		t.Fatalf("a short is not a long: got %q", r)
	}
	exit := riskOffLong(score(-2))
	exit.Side = ""
	if ok, _ := EvaluateEntry(exit, c); !ok {
		t.Fatal("non-opens are never gated")
	}
}

// TestEvidence_RiskOffAddScoreIsChecked_716 pins what #716's -2 floor relies
// on: an add's regime score is compared to the tool output like an open's,
// and a missing score is refused.
func TestEvidence_RiskOffAddScoreIsChecked_716(t *testing.T) {
	calls := []ToolCall{acct(`{"symbol":"SHEL","qty":26,"avg_cost":93.92885,"market_price":95.82}`), sc("SPY", 1, 1, 0, 0, 0, 0),
		scFull("SHEL", 95.41, 94.174, 91.0162, 83.2048, 56.9867, 1.6467), regime("eu", -2, "RISK_OFF")}
	for name, reg := range map[string]string{
		"score differs from the tool": `"regime":{"score":-1,"label":"RISK_OFF"}`,
		"score missing":               `"regime":{"label":"RISK_OFF"}`,
	} {
		t.Run(name, func(t *testing.T) {
			res := strings.Replace(string(addResult(addEv)), `"regime":{"score":2,"label":"RISK_ON"}`, reg, 1)
			r := CheckAnalysisEvidence([]byte(res), calls, evCfg(), []string{"SHEL"}, nil)
			if r == nil || r.Reason != ReasonProposalMismatch {
				t.Fatalf("want %s, got %+v", ReasonProposalMismatch, r)
			}
		})
	}
}

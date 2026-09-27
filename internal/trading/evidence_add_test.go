package trading

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Amendment 2026-09-25 (trading-entry-policy design): an `intent: add`
// proposal carries add_evidence, and the gate proves every number of it —
// avg_cost against this step's get_account_summary position, the six
// indicators against this step's mcp__ta__scorecard output for the symbol —
// so the filter's pullback rule judges tool values, not model prose.

func scFull(symbol string, last, sma20, sma50, sma200, rsi, atr float64) ToolCall {
	return ToolCall{Name: "mcp__ta__scorecard", Input: fmt.Sprintf(`{"symbol":%q,"region":"eu"}`, symbol), OK: true,
		Output: fmt.Sprintf(`{"symbol":%q,"total":3,"trend":2,"momentum":-1,"macro":2,"last_close":%g,"sma20":%g,"sma50":%g,"sma200":%g,"rsi14":%g,"atr14":%g}`,
			symbol, last, sma20, sma50, sma200, rsi, atr)}
}

const addEv = `"add_evidence":{"avg_cost":93.92885,"last_close":95.41,"sma20":94.174,"sma50":91.0162,"sma200":83.2048,"rsi14":56.9867,"atr14":1.6467}`

func addResult(evidence string) []byte {
	prop := `{"symbol":"SHEL","intent":"add","action":"BUY","region":"eu","scorecard":{"total":3,"trend":2,"momentum":-1,"macro":2},"regime":{"score":2,"label":"RISK_ON"}`
	if evidence != "" {
		prop += "," + evidence
	}
	return []byte(`{"proposals":[` + prop + `}],"holdings_review":[{"symbol":"SHEL","last_close":95.41,"sma50":91.0162,"verdict":"hold"}]}`)
}

func addCalls(shelHeld bool) []ToolCall {
	positions := []string{}
	if shelHeld {
		positions = append(positions, `{"symbol":"SHEL","qty":26,"avg_cost":93.92885,"market_price":95.82}`)
	}
	return []ToolCall{acct(positions...), sc("SPY", 1, 1, 0, 0, 0, 0),
		scFull("SHEL", 95.41, 94.174, 91.0162, 83.2048, 56.9867, 1.6467), regime("eu", 2, "RISK_ON")}
}

func TestEvidence_AProvenAddPasses(t *testing.T) {
	assert.Nil(t, CheckAnalysisEvidence(addResult(addEv), addCalls(true), evCfg(), []string{"SHEL"}, nil))
}

func TestEvidence_AddRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		result []byte
		calls  []ToolCall
		reason string
	}{
		"add on a symbol not held": {addResult(addEv), addCalls(false), ReasonAddNotHeld},
		"add without evidence":     {addResult(""), addCalls(true), ReasonAddEvidenceMissing},
		"avg_cost the broker did not report": {addResult(strings.Replace(addEv, `"avg_cost":93.92885`, `"avg_cost":90`, 1)),
			addCalls(true), ReasonAddEvidenceMismatch},
		"an indicator the scorecard did not return": {addResult(strings.Replace(addEv, `"rsi14":56.9867`, `"rsi14":50`, 1)),
			addCalls(true), ReasonAddEvidenceMismatch},
		"a missing indicator": {addResult(strings.Replace(addEv, `,"atr14":1.6467`, ``, 1)),
			addCalls(true), ReasonAddEvidenceMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			// The holdings review needs SHEL held; without it, the add check
			// must still be what names the problem.
			result := tc.result
			if name == "add on a symbol not held" {
				result = []byte(strings.Replace(string(result), `"holdings_review":[{"symbol":"SHEL","last_close":95.41,"sma50":91.0162,"verdict":"hold"}]`, `"holdings_review":[]`, 1))
			}
			r := CheckAnalysisEvidence(result, tc.calls, evCfg(), []string{"SHEL"}, nil)
			require.NotNil(t, r, "expected refusal")
			assert.Equal(t, tc.reason, r.Reason, r.Detail)
			assert.Contains(t, r.Detail, "SHEL")
		})
	}
}

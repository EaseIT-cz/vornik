package verifier

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/trading"
)

// Amendment 2026-09-25 (trading-entry-policy design): `intent: add` is a
// known intent. With additions off it is rejected (never an error — the
// strategist may propose one); with pullback additions on, the rule
// (trading.PullbackAdditions.Reason) runs on the carried add_evidence and the
// proposal's own limit_price, on both the proposals and approved arrays.

const goodAdd = `{"symbol":"SHEL","intent":"add","action":"BUY","order_type":"LMT","qty":6,"limit_price":95.1,"stop_loss_price":91.9,` +
	`"add_evidence":{"avg_cost":90,"last_close":95,"sma20":94,"sma50":91,"sma200":83,"rsi14":52,"atr14":1.6}}`

func addFilterPolicy() trading.EntryPolicy {
	return trading.EntryPolicy{Enabled: true, AllowedSymbols: []string{"SHEL", "MSFT"}, LongOnly: true,
		MaxRiskUSD: 50, MinNotionalUSD: 500, MaxEntriesPerTick: 1,
		PullbackAdditions: trading.PullbackAdditions{Enabled: true, MaxDistanceATR: 1, RSIMin: 45, RSIMax: 60}}
}

func filterOne(t *testing.T, field, proposal string, policy trading.EntryPolicy) (kept []json.RawMessage, rejections string) {
	t.Helper()
	out, err := FilterTradingEntryPolicy([]byte(`{"`+field+`":[`+proposal+`]}`), policy)
	require.NoError(t, err)
	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &decoded))
	require.NoError(t, json.Unmarshal(decoded[field], &kept))
	return kept, string(decoded["entry_policy_rejections"])
}

func TestEntryPolicyAddKeptWhenItQualifies(t *testing.T) {
	for _, field := range []string{"proposals", "approved"} {
		kept, rej := filterOne(t, field, goodAdd, addFilterPolicy())
		require.Len(t, kept, 1, "%s: a qualifying add was dropped: %s", field, rej)
	}
}

func TestEntryPolicyAddRejectedWhenAdditionsAreOff(t *testing.T) {
	p := addFilterPolicy()
	p.PullbackAdditions = trading.PullbackAdditions{}
	p.NoPositionAdditions = true
	kept, rej := filterOne(t, "proposals", goodAdd, p)
	require.Empty(t, kept)
	require.Contains(t, rej, "additions_disabled")
}

func TestEntryPolicyAddConditionsOnBothArrays(t *testing.T) {
	for reason, proposal := range map[string]string{
		"add_evidence_missing":     `{"symbol":"SHEL","intent":"add","action":"BUY","order_type":"LMT","qty":6,"limit_price":95.1,"stop_loss_price":91.9}`,
		"add_not_in_profit":        goodAddWith(`"avg_cost":90`, `"avg_cost":95`),
		"add_rsi_out_of_band":      goodAddWith(`"rsi14":52`, `"rsi14":66`),
		"add_limit_outside_band":   goodAddWith(`"limit_price":95.1,"stop_loss_price":91.9`, `"limit_price":95.7,"stop_loss_price":92.7`),
		"entry_risk_exceeded":      goodAddWith(`"qty":6`, `"qty":20`),
		"entry_symbol_not_allowed": goodAddWith(`"symbol":"SHEL"`, `"symbol":"NVDA"`),
	} {
		for _, field := range []string{"proposals", "approved"} {
			kept, rej := filterOne(t, field, proposal, addFilterPolicy())
			require.Empty(t, kept, "%s/%s kept", reason, field)
			require.Contains(t, rej, reason, field)
		}
	}
}

// An add is an entry: it counts toward max_entries_per_tick with opens.
func TestEntryPolicyAddCountsAsAnEntry(t *testing.T) {
	open := `{"symbol":"MSFT","intent":"open","action":"BUY","order_type":"LMT","qty":2,"limit_price":500,"stop_loss_price":480}`
	out, err := FilterTradingEntryPolicy([]byte(`{"proposals":[`+open+`,`+goodAdd+`]}`), addFilterPolicy())
	require.NoError(t, err)
	require.Contains(t, string(out), "entry_count_or_duplicate")
	var decoded struct{ Proposals []policyProposal }
	require.NoError(t, json.Unmarshal(out, &decoded))
	require.Len(t, decoded.Proposals, 1)
}

// goodAddWith is goodAdd with one fragment replaced.
func goodAddWith(old, repl string) string {
	if !strings.Contains(goodAdd, old) {
		panic("fixture drift: " + old)
	}
	return strings.Replace(goodAdd, old, repl, 1)
}

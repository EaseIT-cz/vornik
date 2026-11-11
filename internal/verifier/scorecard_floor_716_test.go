package verifier

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #716: the verifier carries regime.score to the floor. At -2 a
// RISK_OFF long with a label but no score is dropped (fail closed), a -1
// one passes, and the drop detail names the score and the threshold.

func riskOffResult(regime string) []byte {
	return []byte(`{"has_proposals":true,"proposals":[{"symbol":"MSFT","intent":"open","action":"BUY","region":"us",
	 "scorecard":{"total":4},"regime":` + regime + `}]}`)
}

func TestFilterTradingProposals_BlockLongAtScore_716(t *testing.T) {
	fc := floorCfgEnabled()
	fc.BlockLongAtScore = -2
	kept := func(regime string) bool {
		out, err := FilterTradingProposals(riskOffResult(regime), fc)
		require.NoError(t, err)
		return strings.Contains(string(out), `"MSFT"`)
	}
	assert.True(t, kept(`{"score":-1,"label":"RISK_OFF","component_count":3}`), "score -1 at -2 passes")
	assert.False(t, kept(`{"score":-2,"label":"RISK_OFF","component_count":3}`), "score -2 at -2 is dropped")
	assert.False(t, kept(`{"label":"RISK_OFF","component_count":3}`), "a missing score at -2 is dropped")

	fc.BlockLongAtScore = -1
	assert.False(t, kept(`{"score":-1,"label":"RISK_OFF","component_count":3}`), "-1 keeps today's label block")
}

func TestClassifyFloorProposal_DetailNamesScoreAndThreshold_716(t *testing.T) {
	fc := floorCfgEnabled()
	fc.BlockLongAtScore = -2
	props := extractFloorProposals(riskOffResult(`{"score":-2,"label":"RISK_OFF","component_count":3}`))
	require.Len(t, props, 1)
	_, reason, detail := classifyFloorProposal(props[0], floorEntryConfig(fc), nil)
	assert.Equal(t, "risk_off_long", reason)
	assert.Contains(t, detail, "score=-2")
	assert.Contains(t, detail, "block_at=-2")
}

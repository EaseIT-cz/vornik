package registry

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestProjectEntryPolicyYAML(t *testing.T) {
	var p Project
	require.NoError(t, yaml.Unmarshal([]byte(`trading:
  entry_policy:
    enabled: true
    allowed_symbols: [MSFT, JPM]
    long_only: true
    no_position_additions: true
    max_risk_usd: 50
    min_notional_usd: 500
    max_entries_per_tick: 1
`), &p))
	require.True(t, p.Trading.EntryPolicy.Enabled)
	require.True(t, p.Trading.EntryPolicy.LongOnly)
	require.Equal(t, []string{"MSFT", "JPM"}, p.Trading.EntryPolicy.AllowedSymbols)
	require.Equal(t, 50.0, p.Trading.EntryPolicy.MaxRiskUSD)
	require.NoError(t, p.Trading.EntryPolicy.Validate())
	p.ID, p.SwarmID, p.DefaultWorkflowID = "p", "s", "w"
	require.NoError(t, p.Validate("p.yaml"))
	p.Trading.EntryPolicy.MaxRiskUSD = 0
	require.ErrorContains(t, p.Validate("p.yaml"), "trading.entry_policy")
}

// Amendment 2026-09-25: the conditional-addition block parses from project
// YAML as the deployed ibkr-trader.yaml spells it, and the unconditional state
// (no_position_additions false, pullback off) is refused at load.
func TestProjectEntryPolicyPullbackAdditionsYAML(t *testing.T) {
	var p Project
	require.NoError(t, yaml.Unmarshal([]byte(`projectId: p
swarmId: s
defaultWorkflowId: w
trading:
  entry_policy:
    enabled: true
    allowed_symbols: [MSFT]
    long_only: true
    no_position_additions: false
    pullback_additions:
      enabled: true
      max_distance_atr: 1.0
      rsi_min: 45
      rsi_max: 60
    max_risk_usd: 50
    min_notional_usd: 500
    max_entries_per_tick: 1
`), &p))
	pa := p.Trading.EntryPolicy.PullbackAdditions
	require.True(t, pa.Enabled)
	require.Equal(t, 1.0, pa.MaxDistanceATR)
	require.Equal(t, 45.0, pa.RSIMin)
	require.Equal(t, 60.0, pa.RSIMax)
	require.NoError(t, p.Validate("p.yaml"))

	p.Trading.EntryPolicy.PullbackAdditions.Enabled = false
	require.ErrorContains(t, p.Validate("p.yaml"), "no_position_additions")
}

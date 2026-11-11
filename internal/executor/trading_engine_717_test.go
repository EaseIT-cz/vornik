package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/trading"
)

// GitHub #717 (#598 step 2): with trading.rules.authority: engine the
// participant fetches the setup inputs, the rule engine decides the opens and
// the model's opens are replaced. Found at: production (2026-10-01 18:12 tick,
// all regions RISK_ON, ASML scorecard 6, no hourly bars fetched, no proposal).

// engineProject is a trading project whose engine authority is on, with the
// v2.1 caps and one allowed symbol, ASML (eu).
func engineProject() *registry.Project {
	p := evidenceProject()
	p.Trading.Watchlist = []string{"ASML", "SPY"}
	p.Trading.EntryPolicy = trading.EntryPolicy{Enabled: true, NoPositionAdditions: true, AllowedSymbols: []string{"ASML"}, MaxRiskUSD: 75,
		MaxEntriesPerTick: 2, MaxPositions: 8, MaxGrossExposureUSD: 15000}
	p.Trading.Rules.Authority = "engine"
	// The floor is live, with ibkr-trader's settings, so a passing test shows
	// the floor keeps the engine's open (design B.3: same thresholds).
	p.Trading.Scorecard.Enabled = true
	p.Trading.Regime.MinComponentCount = map[string]int{"us": 6, "eu": 5, "apac": 4}
	p.Trading.EntryPolicy.LongOnly = true
	return p
}

// engineAudit is a 2026-10-01-style tick: RISK_ON everywhere, SPY in trend,
// ASML qualifying on the daily scorecard, and no hourly bars, quote or
// fundamentals fetched by the model.
func engineAudit() *evidenceAuditRepo {
	return &evidenceAuditRepo{entries: []*persistence.ToolAuditEntry{
		{ToolName: "mcp__broker__get_account_summary", ToolOutput: `{"positions":[]}`, Outcome: "ok"},
		{ToolName: "mcp__broker__get_orders", ToolOutput: `{"open":[]}`, Outcome: "ok"},
		{ToolName: "mcp__ta__scorecard", ToolInput: `{"symbol":"SPY","region":"us"}`, Outcome: "ok",
			ToolOutput: `{"total":3,"trend":2,"momentum":1,"macro":0,"last_close":700,"sma20":690,"sma50":680,"sma200":600,"rsi14":60,"atr14":8}`},
		{ToolName: "mcp__ta__scorecard", ToolInput: `{"symbol":"ASML","region":"eu"}`, Outcome: "ok",
			ToolOutput: `{"total":6,"trend":2,"momentum":2,"macro":2,"last_close":1000,"sma20":980,"sma50":950,"sma200":900,"rsi14":62,"atr14":15}`},
		{ToolName: "mcp__ta__regime", ToolInput: `{"region":"eu"}`, Outcome: "ok",
			ToolOutput: `{"region":"eu","score":2,"label":"RISK_ON","stale":false,"component_count":5}`},
	}}
}

const engineModelResult = `{"proposals":[{"symbol":"ASML","intent":"open","action":"BUY","qty":1,"limit_price":999,"region":"eu",
   "scorecard":{"total":6,"trend":2,"momentum":2,"macro":2},"regime":{"score":2,"label":"RISK_ON","stale":false,"component_count":5}}],
 "has_proposals":true,"holdings_review":[],
 "entry_funnel":[{"symbol":"ASML","intent":"open","status":"proposed","gate":"none","reason":"model"}]}`

// fakeTools answers the engine's fetches from recorded responses.
type fakeTools struct {
	mu       sync.Mutex
	calls    []string
	fail     map[string]bool
	inflight map[string]int // per service
	peak     map[string]int
	delay    time.Duration
}

func toolService(tool string) string {
	if tool == "fundamentals_snapshot" {
		return "news"
	}
	return "broker"
}

func (f *fakeTools) call(_ context.Context, _, projectID, tool string, _ map[string]any) (string, error) {
	svc := toolService(tool)
	f.mu.Lock()
	if f.inflight == nil {
		f.inflight, f.peak = map[string]int{}, map[string]int{}
	}
	f.inflight[svc]++
	f.peak[svc] = max(f.peak[svc], f.inflight[svc])
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inflight[svc]--; f.mu.Unlock() }()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	f.calls = append(f.calls, tool+"|"+projectID)
	f.mu.Unlock()
	if f.fail[tool] {
		return "", errors.New("down")
	}
	switch tool {
	case "get_quote":
		b, _ := os.ReadFile("../trading/testdata/get_quote_717.json")
		// The recorded AZN quote, moved to ASML's price level.
		return strings.NewReplacer(`"AZN"`, `"ASML"`, "186.98", "1000.00", "187.03", "1000.10", "186.99", "1000.05").Replace(string(b)), nil
	case "fundamentals_snapshot":
		b, _ := os.ReadFile("../trading/testdata/fundamentals_snapshot_717.json")
		return string(b), nil
	case "get_historical_bars":
		// 30 completed bars rising +3 / -2: the last close sits above its
		// SMA20 and RSI14 is about 60, inside [35,75].
		closes, c := make([]map[string]any, 30), 960.0
		for i := range closes {
			if i%2 == 0 {
				c += 3
			} else {
				c -= 2
			}
			closes[i] = map[string]any{"close": c}
		}
		b, _ := json.Marshal(map[string]any{"bar_size": "1 hour", "bars": closes})
		return string(b), nil
	}
	return "", errors.New("unexpected tool " + tool)
}

func engineExecutor(t *testing.T, tools *fakeTools, sma50 float64) (*Executor, *funnelRepoStub, *shadowRepoStub) {
	t.Helper()
	prevCall, prevSMA := engineCallTool, engineSMA50
	engineCallTool = tools.call
	engineSMA50 = func(context.Context, *Executor, *registry.Project, []byte) map[string]float64 {
		if sma50 <= 0 {
			return map[string]float64{}
		}
		return map[string]float64{"ASML": sma50}
	}
	t.Cleanup(func() { engineCallTool, engineSMA50 = prevCall, prevSMA })
	funnel, shadow := &funnelRepoStub{}, &shadowRepoStub{}
	e := &Executor{logger: zerolog.Nop(), workflows: &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": engineProject()}},
		auditRepo: engineAudit(), tradingFunnelRepo: funnel, tradingShadowRepo: shadow}
	return e, funnel, shadow
}

func engineOutcome() *StepOutcome {
	in := evidenceOutcome()
	in.ResultBytes = []byte(engineModelResult)
	return in
}

func proposals(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var obj struct {
		Proposals []map[string]any `json:"proposals"`
	}
	require.NoError(t, json.Unmarshal(b, &obj))
	return obj.Proposals
}

func TestEngine_DecidesTheOpens_717(t *testing.T) {
	pinSession(t, true)
	tools := &fakeTools{}
	e, funnel, shadow := engineExecutor(t, tools, 950)
	in := engineOutcome()
	v := e.tradingFloorParticipant(context.Background(), in)
	require.False(t, v.Refused, v.Reason)
	props := proposals(t, in.ResultBytes)
	require.Len(t, props, 1, string(in.ResultBytes))
	assert.Equal(t, "rule_engine", props[0]["source"], "the model's open is replaced by the engine's")
	assert.Equal(t, "ASML", props[0]["symbol"])
	assert.NotEqual(t, 999.0, props[0]["limit_price"])
	// every fetch carries the project header, and all three tools ran
	assert.ElementsMatch(t, []string{"get_historical_bars|trade", "get_quote|trade", "fundamentals_snapshot|trade"}, tools.calls)
	require.Len(t, funnel.rows, 1)
	assert.Equal(t, "proposed", funnel.rows[0].Status, "the funnel records the engine's decision")
	require.NotNil(t, funnel.rows[0].RefClose, "forward outcomes need the reference close")
	require.Len(t, shadow.rows, 1)
	assert.Equal(t, "proposed", shadow.rows[0].EngineStatus)
	assert.Equal(t, "proposed", shadow.rows[0].ModelStatus, "the model column comes from the original bytes")
}

func TestEngine_FetchFailureMeansNoOpen_717(t *testing.T) {
	pinSession(t, true)
	e, _, shadow := engineExecutor(t, &fakeTools{fail: map[string]bool{"get_quote": true}}, 950)
	in := engineOutcome()
	v := e.tradingFloorParticipant(context.Background(), in)
	require.False(t, v.Refused, v.Reason)
	assert.Empty(t, proposals(t, in.ResultBytes), "a failed quote fetch is insufficient_inputs, and the model's open is gone")
	require.Len(t, shadow.rows, 1)
	assert.Equal(t, "insufficient_inputs", shadow.rows[0].EngineGate)
}

func TestEngine_LimitBelowDaemonSMA50Dropped_717(t *testing.T) {
	pinSession(t, true)
	for name, sma := range map[string]float64{"below": 1005, "unknown": 0} {
		t.Run(name, func(t *testing.T) {
			e, funnel, _ := engineExecutor(t, &fakeTools{}, sma)
			in := engineOutcome()
			v := e.tradingFloorParticipant(context.Background(), in)
			require.False(t, v.Refused, v.Reason)
			assert.Empty(t, proposals(t, in.ResultBytes))
			require.Len(t, funnel.rows, 1)
			assert.Equal(t, "daily_trend", funnel.rows[0].Gate)
		})
	}
}

func TestEngine_ShadowAuthorityUnchanged_717(t *testing.T) {
	pinSession(t, true)
	tools := &fakeTools{}
	e, _, _ := engineExecutor(t, tools, 950)
	e.workflows.(*MockWorkflowResolver).projects["trade"].Trading.Rules.Authority = ""
	in := engineOutcome()
	_ = e.tradingFloorParticipant(context.Background(), in)
	// The recorded shadow rows themselves are pinned against the pre-#717
	// behaviour by TestShadow_RecordsEngineBesideModel_598, unchanged.
	assert.Empty(t, tools.calls, "shadow mode makes no fetch")
	for _, p := range proposals(t, in.ResultBytes) {
		assert.NotEqual(t, "rule_engine", p["source"], "shadow mode never writes an engine proposal")
	}
}

func TestEngine_OutOfSessionDoesNothing_717(t *testing.T) {
	pinSession(t, false)
	tools := &fakeTools{}
	e, _, _ := engineExecutor(t, tools, 950)
	in := engineOutcome()
	_ = e.tradingFloorParticipant(context.Background(), in)
	assert.Empty(t, tools.calls, "out of session the engine is never called")
	for _, p := range proposals(t, in.ResultBytes) {
		assert.NotEqual(t, "rule_engine", p["source"])
	}
}

func TestEngine_ErrorDropsModelOpens_717(t *testing.T) {
	pinSession(t, true)
	e, funnel, shadow := engineExecutor(t, &fakeTools{}, 950)
	in := engineOutcome()
	// A proposals value that is not an array cannot be rewritten row by row.
	in.ResultBytes = []byte(`{"proposals":[{"symbol":"ASML","intent":"open","action":"BUY","region":"eu",
	  "scorecard":{"total":6,"trend":2,"momentum":2,"macro":2},"regime":{"score":2,"label":"RISK_ON"}}],
	  "has_proposals":true,"holdings_review":[],"entry_funnel":{"bad":1}}`)
	v := e.tradingFloorParticipant(context.Background(), in)
	require.False(t, v.Refused, v.Reason)
	assert.Empty(t, proposals(t, in.ResultBytes), "an engine error never lets the model's open through")
	assert.Empty(t, shadow.rows, "no shadow row on an engine error")
	// The original funnel is what recordFunnel reads on this path; here it
	// is malformed, so nothing is recorded rather than an engine row.
	assert.Empty(t, funnel.rows)
}

func TestEngine_FetchConcurrencyBounded_717(t *testing.T) {
	tools := &fakeTools{delay: 20 * time.Millisecond}
	prev := engineCallTool
	engineCallTool = tools.call
	t.Cleanup(func() { engineCallTool = prev })
	e := &Executor{logger: zerolog.Nop()}
	syms := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	_ = e.fetchSetupInputs(context.Background(), "trade", syms)
	// 16 broker and 8 news calls, each held 20 ms: an unbounded fetch peaks
	// at 16 and 8; the semaphore holds both at 4.
	assert.Equal(t, 4, tools.peak["broker"], "at most 4 broker calls in flight")
	assert.Equal(t, 4, tools.peak["news"], "at most 4 news calls in flight")
	assert.Len(t, tools.calls, 24)
}

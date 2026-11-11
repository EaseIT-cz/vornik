package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/trading"
	"vornik.io/vornik/internal/trading/rules"
)

// The rule engine decides the opens (GitHub #717, #598 step 2; trading
// entry-policy design, Amendment 2026-11-10 #717). With
// trading.rules.authority: engine, the participant fetches the setup inputs
// the model skipped, runs rules.Decide, and replaces the model's opens with
// the engine's. Every downstream check still runs on the result.

const (
	engineCallTimeout  = 10 * time.Second
	engineFetchTimeout = 20 * time.Second
	engineBodyCap      = 4 << 20
)

// engineSlots bounds in-flight fetches per service, process-wide, so
// concurrent ticks cannot saturate the broker or news sidecar.
var engineSlots = map[string]chan struct{}{"broker": make(chan struct{}, 4), "news": make(chan struct{}, 4)}

// engineCallTool is the fetch seam; tests replace it.
var engineCallTool = callMCPTool

// engineSMA50 reads the daemon's own daily SMA50 for the result's long
// opens, the value the entry_gate_consistent verifier reads. A seam for tests.
var engineSMA50 = func(ctx context.Context, e *Executor, p *registry.Project, b []byte) map[string]float64 {
	out := map[string]float64{}
	for sym, ind := range e.entryGateIndicators(ctx, p, b) {
		out[sym] = ind.SMA50
	}
	return out
}

var (
	engineTicksTotal *prometheus.CounterVec
	engineFetchTotal *prometheus.CounterVec
)

// RegisterEngineMetrics registers the engine's two counters.
func RegisterEngineMetrics(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	engineTicksTotal = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Subsystem: "trading", Name: "engine_ticks_total",
		Help: "Ticks the rule engine decided, by result: opens, no_opens or error.",
	}, []string{"result"})
	engineFetchTotal = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Subsystem: "trading", Name: "engine_fetch_total",
		Help: "Setup-input fetches the rule engine attempted, by tool and result: ok, failed, or cancelled (no slot before the deadline).",
	}, []string{"tool", "result"})
}

func incEngine(v *prometheus.CounterVec, labels ...string) {
	if v != nil {
		v.WithLabelValues(labels...).Inc()
	}
}

// newsBaseURL is the news sidecar's base URL, as brokerBaseURL is the
// broker's.
func newsBaseURL() string {
	if u := os.Getenv("VORNIK_NEWS_BASE_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:8791"
}

// engineCaps projects the project's entry policy onto the engine's caps.
func engineCaps(p *registry.Project) rules.Caps {
	ep := p.Trading.EntryPolicy
	return rules.Caps{MaxRiskUSD: ep.MaxRiskUSD, MaxEntriesPerTick: ep.MaxEntriesPerTick,
		MaxPositions: ep.MaxPositions, MaxGrossExposureUSD: ep.MaxGrossExposureUSD, DailyLossPauseUSD: ep.DailyLossPauseUSD}
}

// engineDecide replaces the model's opens in in.ResultBytes with the
// engine's and records the funnel and the shadow. An error means the result
// could not be rewritten at all; the caller refuses the step.
func (e *Executor) engineDecide(ctx context.Context, in *StepOutcome, calls []trading.ToolCall, p *registry.Project) error {
	original := in.ResultBytes
	now := evidenceNow().UTC()
	pol, caps := enginePolicy(p), engineCaps(p)
	universe := watchlisted(p)
	build := func(cs []trading.ToolCall) rules.Inputs {
		ins := trading.ShadowInputs(cs, universe, now, 0.01)
		e.addOrderState(ctx, &ins, p.ID, now, pol)
		return ins
	}
	ins := build(calls)
	if fetched := e.fetchSetupInputs(ctx, p.ID, rules.SetupCandidates(ins, pol, caps)); len(fetched) > 0 {
		ins = build(append(append([]trading.ToolCall{}, calls...), fetched...))
	}
	d := rules.Decide(ins, pol, caps)
	out, err := trading.RewriteForEngine(original, d, ins)
	if err != nil {
		// Engine error: no opens. The model's opens are dropped, never a fallback.
		incEngine(engineTicksTotal, "error")
		e.logger.Warn().Err(err).Str("task_id", in.Task.ID).Msg("rule engine: tick not decided; no opens")
		e.recordFunnel(ctx, in)
		dropped, derr := trading.DropOpenProposals(original)
		if derr != nil {
			return fmt.Errorf("schema violation: rule engine: strategist result is not a JSON object: %w", derr)
		}
		in.ResultBytes = dropped
		return nil
	}
	if len(d.Entries) > 0 {
		var dropped []string
		out, dropped = trading.DropOpensBelowSMA50(out, engineSMA50(ctx, e, p, out))
		if len(dropped) > 0 {
			e.logger.Warn().Str("task_id", in.Task.ID).Strs("symbols", dropped).
				Msg("rule engine: open dropped, limit below (or no) daemon daily SMA50")
		}
	}
	in.ResultBytes = out
	e.recordFunnel(ctx, in)
	e.writeShadow(ctx, in, engineRows(out), original)
	if hasOpen(out) {
		incEngine(engineTicksTotal, "opens")
	} else {
		incEngine(engineTicksTotal, "no_opens")
	}
	return nil
}

// fetchSetupInputs fetches hourly bars, a quote and fundamentals for each
// symbol and returns the valid ones as tool calls the shadow parsers read.
func (e *Executor) fetchSetupInputs(ctx context.Context, projectID string, symbols []string) []trading.ToolCall {
	if len(symbols) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, engineFetchTimeout)
	defer cancel()
	type job struct {
		service, base, tool string
		args                map[string]any
	}
	var jobs []job
	for _, sym := range symbols {
		jobs = append(jobs,
			job{"broker", brokerBaseURL(), "get_historical_bars", map[string]any{"symbol": sym, "duration": "10 D", "bar_size": "1 hour"}},
			job{"broker", brokerBaseURL(), "get_quote", map[string]any{"symbol": sym}},
			job{"news", newsBaseURL(), "fundamentals_snapshot", map[string]any{"symbol": sym}})
	}
	var (
		mu  sync.Mutex
		out []trading.ToolCall
		wg  sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			select {
			case engineSlots[j.service] <- struct{}{}:
				defer func() { <-engineSlots[j.service] }()
			case <-ctx.Done():
				incEngine(engineFetchTotal, j.tool, "cancelled")
				return
			}
			cctx, ccancel := context.WithTimeout(ctx, engineCallTimeout)
			defer ccancel()
			text, err := engineCallTool(cctx, j.base, projectID, j.tool, j.args)
			if err != nil || !trading.ValidFetched(j.tool, text) {
				incEngine(engineFetchTotal, j.tool, "failed")
				return
			}
			incEngine(engineFetchTotal, j.tool, "ok")
			input, _ := json.Marshal(j.args)
			mu.Lock()
			out = append(out, trading.ToolCall{Name: "daemon__" + j.tool, Input: string(input), Output: text, OK: true})
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	return out
}

// callMCPTool calls one read-only tool on a loopback MCP sidecar
// (POST /message, JSON-RPC) and returns the tool's text result. The URL's
// own loopback host is dialled, redirects are never followed and the body
// is read through a cap.
func callMCPTool(ctx context.Context, baseURL, projectID, tool string, args map[string]any) (string, error) {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/message", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if projectID != "" {
		req.Header.Set("X-Project-ID", projectID)
	}
	client := dialguard.LoopbackURLGuard(baseURL, nil).NoRedirectHTTPClient(0)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %d", tool, resp.StatusCode)
	}
	raw, err := dialguard.ReadBoundedBody(resp.Body, engineBodyCap)
	if err != nil {
		return "", err
	}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("%s envelope: %w", tool, err)
	}
	if env.Error != nil {
		return "", fmt.Errorf("%s: %s", tool, env.Error.Message)
	}
	if env.Result.IsError || len(env.Result.Content) == 0 {
		return "", fmt.Errorf("%s: empty or error result", tool)
	}
	return env.Result.Content[0].Text, nil
}

// watchlisted is the engine's universe: the allowed symbols that are also on
// the watchlist, which the SMA50 check and the watchlist verifier cover.
func watchlisted(p *registry.Project) []string {
	on := map[string]bool{}
	for _, s := range p.Trading.Watchlist {
		on[strings.ToUpper(s)] = true
	}
	var out []string
	for _, s := range p.Trading.EntryPolicy.AllowedSymbols {
		if on[strings.ToUpper(s)] {
			out = append(out, s)
		}
	}
	return out
}

// engineRows reads the open funnel rows of a rewritten result.
func engineRows(b []byte) []rules.FunnelRow {
	rows, ok := trading.ParseFunnel(b)
	if !ok {
		return nil
	}
	out := make([]rules.FunnelRow, 0, len(rows))
	for _, r := range rows {
		if strings.EqualFold(r.Intent, "open") {
			out = append(out, rules.FunnelRow{Symbol: strings.ToUpper(r.Symbol), Intent: "open", Status: r.Status, Gate: r.Gate})
		}
	}
	return out
}

func hasOpen(b []byte) bool {
	var obj struct {
		Proposals []struct {
			Intent string `json:"intent"`
		} `json:"proposals"`
	}
	if json.Unmarshal(b, &obj) != nil {
		return false
	}
	for _, p := range obj.Proposals {
		if strings.EqualFold(p.Intent, "open") {
			return true
		}
	}
	return false
}
